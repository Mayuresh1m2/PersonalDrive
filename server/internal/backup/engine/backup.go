// Package engine orchestrates backups, restores, pruning and checks on top
// of the repository format.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/chunker"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
)

// Source is one directory tree to back up, stored under Name at the root of
// each snapshot.
type Source struct {
	Name    string
	Path    string
	Exclude []string // relative paths to skip (and everything under them)
	Hot     bool     // keep content in hot packs (small metadata like the DB)
}

type BackupOptions struct {
	Sources    []Source
	Host       string
	Tags       []string
	StagingDir string // local scratch for packs before upload (laptop SSD)
	// CheckpointPacks writes an index file every N uploaded packs so an
	// interrupted first backup (days long, for TBs over home upload speeds)
	// resumes without re-uploading. 0 = every 20 packs.
	CheckpointPacks int
	Uploaders       int
	Progress        func(Progress)
	Program         string
}

type Progress struct {
	Phase       string `json:"phase"`
	Files       int64  `json:"files"`
	Bytes       int64  `json:"bytes"`
	BytesNew    int64  `json:"bytes_new"`
	Uploaded    int64  `json:"uploaded"`
	PacksQueued int64  `json:"packs_queued"`
	Current     string `json:"current,omitempty"`
}

// Backup creates a snapshot of opts.Sources. Unchanged files (same size,
// mtime and inode as in the previous snapshot) are not re-read.
func Backup(ctx context.Context, r *repo.Repo, opts BackupOptions) (*repo.Snapshot, error) {
	start := time.Now()
	refresh, unlock, err := r.Lock(ctx, "backup")
	if err != nil {
		return nil, err
	}
	defer unlock()
	stopRefresh := keepFresh(ctx, refresh)
	defer stopRefresh()

	if err := r.LoadIndex(ctx); err != nil {
		return nil, fmt.Errorf("loading index: %w", err)
	}
	if opts.Host == "" {
		opts.Host, _ = os.Hostname()
	}
	if opts.CheckpointPacks <= 0 {
		opts.CheckpointPacks = 20
	}
	if opts.Uploaders <= 0 {
		opts.Uploaders = 2
	}
	os.RemoveAll(opts.StagingDir) // leftovers from an interrupted run
	if err := os.MkdirAll(opts.StagingDir, 0o700); err != nil {
		return nil, err
	}

	var parentRoot *repo.Tree
	var parentID string
	if snaps, err := r.Snapshots(ctx); err != nil {
		return nil, err
	} else {
		for i := len(snaps) - 1; i >= 0; i-- {
			if snaps[i].Complete && snaps[i].Host == opts.Host {
				parentID = snaps[i].ID
				if parentRoot, err = r.LoadTree(ctx, snaps[i].Tree); err != nil {
					return nil, fmt.Errorf("loading previous snapshot: %w", err)
				}
				break
			}
		}
	}

	b := &backuper{r: r, opts: opts, p: newPacker(ctx, r, opts), inodes: map[[2]uint64]inodeEntry{}}
	b.lastProgress = time.Now()
	root := &repo.Tree{}
	for _, src := range opts.Sources {
		var parentTree *repo.Tree
		if parentRoot != nil {
			if n := parentRoot.Find(src.Name); n != nil && n.Subtree != "" {
				parentTree, _ = r.LoadTree(ctx, n.Subtree)
			}
		}
		st, err := os.Stat(src.Path)
		if err != nil {
			b.p.abort()
			return nil, fmt.Errorf("source %s: %w", src.Name, err)
		}
		id, err := b.dir(ctx, src, src.Path, "", parentTree)
		if err != nil {
			b.p.abort()
			return nil, err
		}
		root.Nodes = append(root.Nodes, repo.Node{Name: src.Name, Type: "dir", MTime: st.ModTime().UTC(), Mode: uint32(st.Mode().Perm()), Subtree: id})
	}
	rootID, err := b.tree(ctx, root)
	if err != nil {
		b.p.abort()
		return nil, err
	}
	b.progress("uploading", "", true)
	if err := b.p.finish(); err != nil {
		return nil, err
	}

	b.stats.PacksUploaded = int(b.p.packsUploaded.Load())
	b.stats.BytesUploaded = b.p.bytesUploaded.Load()
	b.stats.BytesStored = b.p.bytesStored.Load()
	b.stats.BytesNew = b.p.bytesNew.Load()
	b.stats.BytesDeduped = b.p.bytesDeduped.Load()
	b.stats.Duration = time.Since(start)
	snap := &repo.Snapshot{
		Time: start.UTC(), Host: opts.Host, Tree: rootID, Parent: parentID, Tags: opts.Tags,
		Stats: b.stats, Program: opts.Program, Complete: true,
	}
	// The snapshot file is the commit point: everything it references is
	// already uploaded and indexed.
	if err := r.SaveSnapshot(ctx, snap); err != nil {
		return nil, err
	}
	b.progress("done", "", true)
	return snap, nil
}

func keepFresh(ctx context.Context, refresh func() error) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	return cancel
}

type inodeEntry struct {
	content []string
	sha     string
}

type backuper struct {
	r            *repo.Repo
	opts         BackupOptions
	p            *packer
	stats        repo.SnapshotStats
	inodes       map[[2]uint64]inodeEntry // hardlinked files are read once
	lastProgress time.Time
}

func (b *backuper) progress(phase, current string, force bool) {
	if b.opts.Progress == nil || (!force && time.Since(b.lastProgress) < time.Second) {
		return
	}
	b.lastProgress = time.Now()
	b.opts.Progress(Progress{
		Phase: phase, Files: b.stats.Files, Bytes: b.stats.BytesScanned, BytesNew: b.p.bytesNew.Load(),
		Uploaded: b.p.bytesUploaded.Load(), PacksQueued: b.p.packsQueued.Load(), Current: current,
	})
}

func excluded(rel string, patterns []string) bool {
	for _, p := range patterns {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// dir backs up one directory and returns its tree id.
func (b *backuper) dir(ctx context.Context, src Source, abs, rel string, parent *repo.Tree) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	tree := &repo.Tree{Nodes: make([]repo.Node, 0, len(entries))}
	for _, e := range entries {
		childRel := e.Name()
		if rel != "" {
			childRel = rel + "/" + e.Name()
		}
		if excluded(childRel, src.Exclude) {
			continue
		}
		childAbs := filepath.Join(abs, e.Name())
		info, err := e.Info()
		if err != nil {
			b.stats.Errors = append(b.stats.Errors, err.Error())
			continue
		}
		var pnode *repo.Node
		if parent != nil {
			pnode = parent.Find(e.Name())
		}
		switch {
		case info.IsDir():
			var ptree *repo.Tree
			if pnode != nil && pnode.Type == "dir" && pnode.Subtree != "" {
				ptree, _ = b.r.LoadTree(ctx, pnode.Subtree)
			}
			id, err := b.dir(ctx, src, childAbs, childRel, ptree)
			if err != nil {
				if ctx.Err() != nil {
					return "", err
				}
				b.stats.Errors = append(b.stats.Errors, fmt.Sprintf("%s: %v", childRel, err))
				continue
			}
			b.stats.Dirs++
			tree.Nodes = append(tree.Nodes, repo.Node{Name: e.Name(), Type: "dir", MTime: info.ModTime().UTC(), Mode: uint32(info.Mode().Perm()), Subtree: id})
		case info.Mode().IsRegular():
			node, err := b.file(ctx, src, childAbs, childRel, info, pnode)
			if err != nil {
				if ctx.Err() != nil {
					return "", err
				}
				b.stats.Errors = append(b.stats.Errors, fmt.Sprintf("%s: %v", childRel, err))
				continue
			}
			tree.Nodes = append(tree.Nodes, node)
		default:
			// symlinks, sockets, devices: the library never contains them
		}
	}
	return b.tree(ctx, tree)
}

func inodeOf(info fs.FileInfo) (dev, ino uint64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), st.Ino
	}
	return 0, 0
}

func (b *backuper) file(ctx context.Context, src Source, abs, rel string, info fs.FileInfo, pnode *repo.Node) (repo.Node, error) {
	dev, ino := inodeOf(info)
	node := repo.Node{
		Name: info.Name(), Type: "file", Size: info.Size(), MTime: info.ModTime().UTC(),
		Mode: uint32(info.Mode().Perm()), Inode: ino,
	}
	b.stats.Files++
	b.stats.BytesScanned += info.Size()
	b.progress("scanning", rel, false)

	// Unchanged since the previous snapshot: reuse its content list.
	if pnode != nil && pnode.Type == "file" && pnode.Size == node.Size && pnode.MTime.Equal(node.MTime) &&
		pnode.Inode == node.Inode && pnode.SHA256 != "" && b.allIndexed(pnode.Content) {
		node.Content, node.SHA256 = pnode.Content, pnode.SHA256
		b.stats.FilesUnchanged++
		return node, nil
	}
	// Another hardlink to a file read earlier in this run.
	if ino != 0 {
		if e, ok := b.inodes[[2]uint64{dev, ino}]; ok {
			node.Content, node.SHA256 = e.content, e.sha
			return node, nil
		}
	}
	f, err := os.Open(abs)
	if err != nil {
		return node, err
	}
	defer f.Close()
	h := sha256.New()
	c := chunker.New(io.TeeReader(f, h), b.r.Config.Chunker, b.r.Gear())
	var read int64
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return node, err
		}
		read += int64(len(chunk))
		id := b.r.Key.ID(chunk)
		if err := b.p.add(ctx, id, repo.BlobData, chunk, src.Hot); err != nil {
			return node, err
		}
		node.Content = append(node.Content, repo.IDString(id))
		b.progress("reading", rel, false)
	}
	b.stats.BytesRead += read
	if read != info.Size() {
		// The file changed while we read it; record what we actually read.
		node.Size = read
	}
	node.SHA256 = hex.EncodeToString(h.Sum(nil))
	if ino != 0 {
		b.inodes[[2]uint64{dev, ino}] = inodeEntry{content: node.Content, sha: node.SHA256}
	}
	return node, nil
}

// allIndexed guards against reusing content whose packs were pruned.
func (b *backuper) allIndexed(ids []string) bool {
	for _, s := range ids {
		id, err := repo.ParseID(s)
		if err != nil || !b.r.Index.Has(id) {
			return false
		}
	}
	return true
}

func (b *backuper) tree(ctx context.Context, t *repo.Tree) (string, error) {
	data := repo.EncodeTree(t)
	id := b.r.Key.ID(data)
	if err := b.p.add(ctx, id, repo.BlobTree, data, true); err != nil {
		return "", err
	}
	return repo.IDString(id), nil
}

// ---- packer ---------------------------------------------------------------------

type packJob struct {
	path string
	info repo.PackInfo
}

// packer groups blobs into hot and cold packs and uploads them in the
// background. Blobs only become part of the index (and the local cache) once
// their pack is uploaded AND recorded in a remote index file.
type packer struct {
	ctx  context.Context
	r    *repo.Repo
	opts BackupOptions

	mu      sync.Mutex
	writers map[bool]*repo.PackWriter
	pending map[repo.BlobID]bool

	jobs     chan packJob
	wg       sync.WaitGroup
	saveMu   sync.Mutex
	errMu    sync.Mutex
	err      error
	idxMu    sync.Mutex
	uploaded []repo.PackInfo

	packsQueued, packsUploaded                         atomic.Int64
	bytesUploaded, bytesStored, bytesNew, bytesDeduped atomic.Int64
}

func newPacker(ctx context.Context, r *repo.Repo, opts BackupOptions) *packer {
	p := &packer{
		ctx: ctx, r: r, opts: opts, writers: map[bool]*repo.PackWriter{}, pending: map[repo.BlobID]bool{},
		jobs: make(chan packJob, opts.Uploaders),
	}
	for i := 0; i < opts.Uploaders; i++ {
		p.wg.Add(1)
		go p.uploader()
	}
	return p
}

func (p *packer) failed() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

func (p *packer) setErr(err error) {
	p.errMu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.errMu.Unlock()
}

func (p *packer) add(ctx context.Context, id repo.BlobID, typ repo.BlobType, plain []byte, hot bool) error {
	if err := p.failed(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.r.Index.Has(id) || p.pending[id] {
		p.mu.Unlock()
		if typ == repo.BlobData {
			p.bytesDeduped.Add(int64(len(plain)))
		}
		return nil
	}
	p.pending[id] = true
	p.mu.Unlock()

	sealed, _ := repo.EncodeBlob(p.r.Key, id, plain)
	if typ == repo.BlobData {
		p.bytesNew.Add(int64(len(plain)))
	}
	p.bytesStored.Add(int64(len(sealed)))

	p.mu.Lock()
	w := p.writers[hot]
	if w == nil {
		var err error
		if w, err = repo.NewPackWriter(p.opts.StagingDir, hot, p.r.Key); err != nil {
			p.mu.Unlock()
			return err
		}
		p.writers[hot] = w
	}
	if err := w.Add(id, typ, sealed, len(plain)); err != nil {
		p.mu.Unlock()
		return err
	}
	var full *repo.PackWriter
	if w.Size() >= p.r.Config.PackSize {
		delete(p.writers, hot)
		full = w
	}
	p.mu.Unlock()
	// Ship outside the lock: it may block until an uploader is free.
	if full != nil {
		return p.ship(full)
	}
	return nil
}

// ship finalizes a pack and queues it (blocks when uploaders are busy, which
// bounds staging disk use).
func (p *packer) ship(w *repo.PackWriter) error {
	path, info, err := w.Finalize()
	if err != nil {
		return err
	}
	p.packsQueued.Add(1)
	select {
	case p.jobs <- packJob{path, info}:
		return nil
	case <-p.ctx.Done():
		os.Remove(path)
		return p.ctx.Err()
	}
}

func (p *packer) uploader() {
	defer p.wg.Done()
	for j := range p.jobs {
		if p.failed() != nil {
			os.Remove(j.path)
			continue
		}
		if err := p.r.UploadPack(p.ctx, j.path, j.info); err != nil {
			p.setErr(fmt.Errorf("uploading pack: %w", err))
			os.Remove(j.path)
			continue
		}
		os.Remove(j.path)
		p.packsUploaded.Add(1)
		p.bytesUploaded.Add(j.info.Size)
		p.idxMu.Lock()
		p.uploaded = append(p.uploaded, j.info)
		var checkpoint []repo.PackInfo
		if len(p.uploaded) >= p.opts.CheckpointPacks {
			checkpoint, p.uploaded = p.uploaded, nil
		}
		p.idxMu.Unlock()
		if checkpoint != nil {
			if err := p.index(p.ctx, checkpoint); err != nil {
				p.setErr(err)
			}
		}
	}
}

// index records uploaded packs in the in-memory index and a remote index
// file. Blobs stay in `pending` too, so dedup checks are never wrong.
func (p *packer) index(ctx context.Context, packs []repo.PackInfo) error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	for _, pk := range packs {
		p.r.Index.AddPack(pk)
	}
	_, err := p.r.SaveIndex(ctx, packs)
	return err
}

// finish flushes partial packs, waits for uploads and writes the final index.
func (p *packer) finish() error {
	p.mu.Lock()
	var partial []*repo.PackWriter
	for hot, w := range p.writers {
		delete(p.writers, hot)
		partial = append(partial, w)
	}
	p.mu.Unlock()
	var shipErr error
	for _, w := range partial {
		if w.Count() == 0 {
			w.Abort()
		} else if err := p.ship(w); err != nil && shipErr == nil {
			shipErr = err
		}
	}
	close(p.jobs)
	p.wg.Wait()
	if shipErr != nil {
		return shipErr
	}
	if err := p.failed(); err != nil {
		return err
	}
	p.idxMu.Lock()
	rest := p.uploaded
	p.uploaded = nil
	p.idxMu.Unlock()
	if len(rest) > 0 {
		if err := p.index(p.ctx, rest); err != nil {
			return err
		}
	}
	return nil
}

func (p *packer) abort() {
	p.mu.Lock()
	for hot, w := range p.writers {
		w.Abort()
		delete(p.writers, hot)
	}
	p.mu.Unlock()
	p.setErr(errors.New("aborted"))
	close(p.jobs)
	p.wg.Wait()
	// Packs already uploaded are still worth indexing: the next run reuses them.
	p.idxMu.Lock()
	rest := p.uploaded
	p.uploaded = nil
	p.idxMu.Unlock()
	if len(rest) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		p.index(ctx, rest)
	}
}
