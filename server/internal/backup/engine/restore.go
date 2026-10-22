package engine

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
)

// ErrThawPending is returned by Restore with NoWait when archived data is
// still being thawed. Run the restore again later; it resumes.
var ErrThawPending = errors.New("archived data is still being restored from cold storage")

type RestoreOptions struct {
	Snapshot string // "latest", id or prefix
	// Targets maps snapshot source names ("library", "meta") to local dirs.
	// Sources without a target are skipped.
	Targets map[string]string
	// Include limits the restore to paths under these prefixes, written as
	// "<source>/<path>", e.g. "library/users/alice/Photos".
	Include []string
	Tier    string // Bulk | Standard | Expedited (S3 archive tiers)
	// ThawDays keeps thawed copies readable this long.
	ThawDays     int
	PollInterval time.Duration
	NoWait       bool
	Verify       bool   // re-hash files that look already restored
	CacheDir     string // local pack cache
	CacheBytes   int64
	Pricing      Pricing
	Local        bool // store is a local disk (no thaw, no egress costs)
	Progress     func(RestoreProgress)
}

type RestoreProgress struct {
	Phase      string `json:"phase"` // planning | thawing | restoring | done
	FilesDone  int    `json:"files_done"`
	FilesTotal int    `json:"files_total"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	PacksReady int    `json:"packs_ready"`
	PacksTotal int    `json:"packs_total"`
	Current    string `json:"current,omitempty"`
}

type plannedFile struct {
	snapPath string
	target   string
	node     repo.Node
}

type plannedDir struct {
	target string
	mtime  time.Time
	mode   uint32
}

type packNeed struct {
	ID       string
	Hot      bool
	Size     int64
	Lo, Hi   int64 // byte span needed
	Class    string
	Readable bool
	Restore  objectstore.RestoreState
}

// RestorePlan describes what a restore will do and cost.
type RestorePlan struct {
	Snapshot       *repo.Snapshot
	Files          int
	Bytes          int64
	AlreadyPresent int
	Packs          int
	ArchivedPacks  int // need thawing (not yet readable)
	ThawingPacks   int // restore already in progress
	ArchivedBytes  int64
	DownloadBytes  int64
	Estimate       Estimate

	files []plannedFile
	dirs  []plannedDir
	packs map[string]*packNeed
}

// RemainingBytes is the size of files still to be written.
func (p *RestorePlan) RemainingBytes() int64 {
	var n int64
	for _, f := range p.files {
		n += f.node.Size
	}
	return n
}

func (p *RestorePlan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "snapshot %s from %s (%s)\n", p.Snapshot.ID[:12], p.Snapshot.Time.Local().Format("2006-01-02 15:04"), p.Snapshot.Host)
	fmt.Fprintf(&b, "files: %d (%s), already restored: %d\n", p.Files, humanBytes(p.Bytes), p.AlreadyPresent)
	fmt.Fprintf(&b, "packs to read: %d, download %s\n", p.Packs, humanBytes(p.DownloadBytes))
	if p.ArchivedPacks+p.ThawingPacks > 0 {
		fmt.Fprintf(&b, "archived packs: %d need thawing (%s), %d already thawing\n", p.ArchivedPacks, humanBytes(p.ArchivedBytes), p.ThawingPacks)
	}
	fmt.Fprintf(&b, "estimated cost: %s\n", p.Estimate)
	for _, n := range p.Estimate.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}

// PlanRestore walks the snapshot and works out which packs are needed, their
// archive state, and the cost. It reads only hot data.
func PlanRestore(ctx context.Context, r *repo.Repo, opts RestoreOptions) (*RestorePlan, error) {
	if err := r.LoadIndex(ctx); err != nil {
		return nil, err
	}
	snap, err := r.FindSnapshot(ctx, opts.Snapshot)
	if err != nil {
		return nil, err
	}
	root, err := r.LoadTree(ctx, snap.Tree)
	if err != nil {
		return nil, err
	}
	plan := &RestorePlan{Snapshot: snap, packs: map[string]*packNeed{}}
	for _, src := range root.Nodes {
		target, ok := opts.Targets[src.Name]
		if !ok || src.Subtree == "" {
			continue
		}
		if !includes(opts.Include, src.Name) {
			continue
		}
		if err := plan.walk(ctx, r, opts, src.Subtree, src.Name, target); err != nil {
			return nil, err
		}
	}
	// Archive state of each needed pack.
	thaw, thawCount := map[string]int64{}, map[string]int{}
	gets := 0
	for _, pn := range plan.packs {
		plan.DownloadBytes += pn.Hi - pn.Lo
		gets++
		if opts.Local || pn.Hot {
			pn.Readable = true
			continue
		}
		info, err := r.Store.Head(ctx, repo.PackKey(pn.ID))
		if err != nil {
			return nil, fmt.Errorf("pack %s: %w", pn.ID[:12], err)
		}
		pn.Class, pn.Restore, pn.Readable = info.StorageClass, info.Restore, info.Readable()
		switch pn.Restore {
		case objectstore.RestoreNone:
			plan.ArchivedPacks++
			plan.ArchivedBytes += pn.Size
			thaw[pn.Class] += pn.Size
			thawCount[pn.Class]++
		case objectstore.RestoreInProgress:
			plan.ThawingPacks++
		}
		if !pn.Readable && pn.Restore == objectstore.RestoreNotNeeded {
			pn.Readable = true
		}
	}
	plan.Packs = len(plan.packs)
	plan.Estimate = opts.Pricing.estimate(opts.Local, thaw, thawCount, tierOr(opts.Tier), gets, plan.DownloadBytes)
	return plan, nil
}

func tierOr(t string) string {
	if t == "" {
		return "Bulk"
	}
	return t
}

func includes(prefixes []string, p string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, pre := range prefixes {
		pre = strings.Trim(pre, "/")
		// Include if p is under pre, or pre is under p (we must descend).
		if p == pre || strings.HasPrefix(p, pre+"/") || strings.HasPrefix(pre, p+"/") {
			return true
		}
	}
	return false
}

func (plan *RestorePlan) walk(ctx context.Context, r *repo.Repo, opts RestoreOptions, treeID, snapPath, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t, err := r.LoadTree(ctx, treeID)
	if err != nil {
		return err
	}
	for _, n := range t.Nodes {
		if strings.ContainsAny(n.Name, "/\\") || n.Name == ".." || n.Name == "." {
			return fmt.Errorf("refusing unsafe name %q in snapshot", n.Name)
		}
		sp := snapPath + "/" + n.Name
		if !includes(opts.Include, sp) {
			continue
		}
		tp := filepath.Join(target, n.Name)
		switch n.Type {
		case "dir":
			plan.dirs = append(plan.dirs, plannedDir{target: tp, mtime: n.MTime, mode: n.Mode})
			if err := plan.walk(ctx, r, opts, n.Subtree, sp, tp); err != nil {
				return err
			}
		case "file":
			plan.Files++
			plan.Bytes += n.Size
			if st, err := os.Stat(tp); err == nil && st.Size() == n.Size && st.ModTime().Equal(n.MTime) && !opts.Verify {
				plan.AlreadyPresent++
				continue
			}
			plan.files = append(plan.files, plannedFile{snapPath: sp, target: tp, node: n})
			for _, s := range n.Content {
				id, err := repo.ParseID(s)
				if err != nil {
					return err
				}
				loc, ok := r.Index.Lookup(id)
				if !ok {
					return fmt.Errorf("%s: blob %s missing from index (run `pdrive backup check`)", sp, s[:12])
				}
				pn := plan.packs[loc.Pack]
				if pn == nil {
					meta, _ := r.Index.Pack(loc.Pack)
					pn = &packNeed{ID: loc.Pack, Hot: meta.Hot, Size: meta.Size, Lo: int64(loc.Offset), Hi: int64(loc.Offset) + int64(loc.Length)}
					plan.packs[loc.Pack] = pn
				}
				pn.Lo = min(pn.Lo, int64(loc.Offset))
				pn.Hi = max(pn.Hi, int64(loc.Offset)+int64(loc.Length))
			}
		}
	}
	return nil
}

// Restore executes a plan: thaws archived packs, waits, then writes files
// (verifying each file's SHA-256) and finally directory mtimes. It is
// resumable: re-running skips files already restored.
func Restore(ctx context.Context, r *repo.Repo, plan *RestorePlan, opts RestoreOptions) error {
	report := func(p RestoreProgress) {
		if opts.Progress != nil {
			opts.Progress(p)
		}
	}
	if opts.ThawDays <= 0 {
		opts.ThawDays = 7
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 15 * time.Minute
	}
	// 1. Thaw.
	var waiting []*packNeed
	for _, pn := range plan.packs {
		if pn.Readable {
			continue
		}
		if pn.Restore == objectstore.RestoreNone {
			if err := r.Store.RequestRestore(ctx, repo.PackKey(pn.ID), tierOr(opts.Tier), opts.ThawDays); err != nil {
				return fmt.Errorf("requesting restore of pack %s: %w", pn.ID[:12], err)
			}
			pn.Restore = objectstore.RestoreInProgress
		}
		waiting = append(waiting, pn)
	}
	// 2. Wait.
	for len(waiting) > 0 {
		report(RestoreProgress{Phase: "thawing", PacksReady: len(plan.packs) - len(waiting), PacksTotal: len(plan.packs)})
		var still []*packNeed
		for _, pn := range waiting {
			info, err := r.Store.Head(ctx, repo.PackKey(pn.ID))
			if err != nil {
				return err
			}
			if info.Readable() {
				pn.Readable = true
			} else {
				still = append(still, pn)
			}
		}
		waiting = still
		if len(waiting) == 0 {
			break
		}
		if opts.NoWait {
			return fmt.Errorf("%w: %d of %d packs ready; run the same restore again later", ErrThawPending, len(plan.packs)-len(waiting), len(plan.packs))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.PollInterval):
		}
	}

	// 3. Files.
	spans := make(map[string][2]int64, len(plan.packs))
	for id, pn := range plan.packs {
		spans[id] = [2]int64{pn.Lo, pn.Hi}
	}
	cache, err := newPackCache(r, opts.CacheDir, opts.CacheBytes, spans)
	if err != nil {
		return err
	}
	defer cache.close()
	var total int64
	for _, f := range plan.files {
		total += f.node.Size
	}
	var done int64
	for i, f := range plan.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := restoreFile(ctx, r, cache, f, opts.Verify); err != nil {
			return fmt.Errorf("%s: %w", f.snapPath, err)
		}
		done += f.node.Size
		report(RestoreProgress{Phase: "restoring", FilesDone: i + 1, FilesTotal: len(plan.files), BytesDone: done, BytesTotal: total, Current: f.snapPath})
	}
	// 4. Directories (deepest first so parents' mtimes stick).
	sort.Slice(plan.dirs, func(i, j int) bool { return len(plan.dirs[i].target) > len(plan.dirs[j].target) })
	for _, d := range plan.dirs {
		os.MkdirAll(d.target, 0o750)
		os.Chtimes(d.target, d.mtime, d.mtime)
	}
	report(RestoreProgress{Phase: "done", FilesDone: len(plan.files), FilesTotal: len(plan.files), BytesDone: total, BytesTotal: total})
	return nil
}

func restoreFile(ctx context.Context, r *repo.Repo, cache *packCache, f plannedFile, verify bool) error {
	if verify {
		if sum, err := hashFile(f.target); err == nil && sum == f.node.SHA256 {
			os.Chtimes(f.target, f.node.MTime, f.node.MTime)
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(f.target), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.target), ".pdrive-restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	for _, s := range f.node.Content {
		id, _ := repo.ParseID(s)
		data, err := cache.blob(ctx, id)
		if err != nil {
			tmp.Close()
			return err
		}
		if _, err := w.Write(data); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.node.SHA256 {
		return fmt.Errorf("restored content hash %s does not match backup %s", got[:12], f.node.SHA256[:12])
	}
	mode := os.FileMode(f.node.Mode).Perm()
	if mode == 0 {
		mode = 0o640
	}
	os.Chmod(tmp.Name(), mode)
	if err := os.Chtimes(tmp.Name(), f.node.MTime, f.node.MTime); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.target)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// packCache keeps recently used pack spans on local disk (LRU by bytes).
// Each pack is fetched as the one byte range the plan needs from it, so
// partial restores only pay egress for what they use.
type packCache struct {
	r     *repo.Repo
	dir   string
	limit int64
	size  int64
	lru   *list.List // of *cachedSpan
	byID  map[string]*list.Element
	spans map[string][2]int64
}

type cachedSpan struct {
	pack   string
	lo, hi int64
	path   string
}

func newPackCache(r *repo.Repo, dir string, limit int64, spans map[string][2]int64) (*packCache, error) {
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "pdrive-restore-cache-"); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1 << 30
	}
	return &packCache{r: r, dir: dir, limit: limit, lru: list.New(), byID: map[string]*list.Element{}, spans: spans}, nil
}

func (c *packCache) close() {
	for e := c.lru.Front(); e != nil; e = e.Next() {
		os.Remove(e.Value.(*cachedSpan).path)
	}
}

func (c *packCache) blob(ctx context.Context, id repo.BlobID) ([]byte, error) {
	loc, ok := c.r.Index.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("blob %s not in index", repo.IDString(id)[:12])
	}
	off, end := int64(loc.Offset), int64(loc.Offset)+int64(loc.Length)
	var span *cachedSpan
	if e, ok := c.byID[loc.Pack]; ok {
		s := e.Value.(*cachedSpan)
		if s.lo <= off && end <= s.hi {
			span = s
			c.lru.MoveToFront(e)
		}
	}
	if span == nil {
		lo, hi := off, end
		if sp, ok := c.spans[loc.Pack]; ok && sp[0] <= off && end <= sp[1] {
			lo, hi = sp[0], sp[1]
		}
		var err error
		if span, err = c.fetch(ctx, loc.Pack, lo, hi); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(span.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sealed := make([]byte, loc.Length)
	if _, err := f.ReadAt(sealed, off-span.lo); err != nil {
		return nil, err
	}
	return repo.DecodeBlob(c.r.Key, id, sealed)
}

func (c *packCache) fetch(ctx context.Context, pack string, lo, hi int64) (*cachedSpan, error) {
	if e, ok := c.byID[pack]; ok {
		c.evict(e)
	}
	p := filepath.Join(c.dir, path.Base(pack)+fmt.Sprintf("-%d", lo))
	err := repo.Retry(ctx, func() error {
		rc, err := c.r.Store.Get(ctx, repo.PackKey(pack), lo, hi-lo)
		if err != nil {
			return err
		}
		defer rc.Close()
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, rc)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
	if err != nil {
		os.Remove(p)
		return nil, err
	}
	s := &cachedSpan{pack: pack, lo: lo, hi: hi, path: p}
	c.byID[pack] = c.lru.PushFront(s)
	c.size += hi - lo
	for c.size > c.limit && c.lru.Len() > 1 {
		c.evict(c.lru.Back())
	}
	return s, nil
}

func (c *packCache) evict(e *list.Element) {
	s := e.Value.(*cachedSpan)
	os.Remove(s.path)
	c.size -= s.hi - s.lo
	c.lru.Remove(e)
	delete(c.byID, s.pack)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
