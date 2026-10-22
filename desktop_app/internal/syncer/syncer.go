// Package syncer implements two-way folder sync between a local directory
// and a folder on the PersonalDrive server.
//
// Each cycle compares three states per path: the local file, the server's
// file, and the "base" (what both looked like after the last successful
// sync). Only sides that differ from the base changed. Rules, in order of
// priority: never lose data; when both sides changed, keep both (the server
// stores the local edit as a "conflict" copy); deletions on one side only
// apply if the other side didn't change; local files removed because of a
// server-side deletion go to a recoverable trash, not oblivion.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/desktop_app/internal/client"
)

// Pair is one synced folder.
type Pair struct {
	Local   string `json:"local"`
	Remote  string `json:"remote"`   // folder in the library ("" = whole library)
	SpaceID int64  `json:"space_id"` // 0 = own library
}

func (p Pair) key() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", p.Local, p.Remote, p.SpaceID)))
	return hex.EncodeToString(h[:8])
}

// Entry is the last-synced state of one path.
type Entry struct {
	IsDir   bool   `json:"dir,omitempty"`
	FileID  int64  `json:"id"`
	Version int64  `json:"v,omitempty"`
	SHA256  string `json:"sha,omitempty"`
	Size    int64  `json:"size,omitempty"`
	MTimeNs int64  `json:"mtime,omitempty"` // local mtime when last synced
}

type state struct {
	Cursor      int64             `json:"cursor"`
	Initialized bool              `json:"initialized"`
	Entries     map[string]*Entry `json:"entries"`
}

type Report struct {
	Uploaded, Downloaded, DeletedLocal, DeletedRemote, Conflicts, Skipped int
	Errors                                                                []string
}

func (r *Report) Changed() bool {
	return r.Uploaded+r.Downloaded+r.DeletedLocal+r.DeletedRemote+r.Conflicts > 0
}

func (r *Report) String() string {
	return fmt.Sprintf("up %d, down %d, deleted locally %d, deleted on server %d, conflicts %d, waiting %d, errors %d",
		r.Uploaded, r.Downloaded, r.DeletedLocal, r.DeletedRemote, r.Conflicts, r.Skipped, len(r.Errors))
}

type Syncer struct {
	C        *client.Client
	Pair     Pair
	StateDir string
	Log      *slog.Logger
	// Settle: files modified more recently than this are still being
	// written and wait for the next cycle.
	Settle time.Duration

	st *state
}

func (s *Syncer) statePath() string { return filepath.Join(s.StateDir, "state-"+s.Pair.key()+".json") }

// TrashDir is where local files removed by server-side deletions go.
func (s *Syncer) TrashDir() string { return filepath.Join(s.StateDir, "trash", s.Pair.key()) }

func (s *Syncer) load() error {
	if s.st != nil {
		return nil
	}
	s.st = &state{Entries: map[string]*Entry{}}
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, s.st); err != nil {
		return fmt.Errorf("corrupt sync state %s: %w", s.statePath(), err)
	}
	if s.st.Entries == nil {
		s.st.Entries = map[string]*Entry{}
	}
	return nil
}

func (s *Syncer) save() error {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

// ---- paths -------------------------------------------------------------------

func (s *Syncer) remotePath(rel string) string {
	if s.Pair.Remote == "" {
		return rel
	}
	if rel == "" {
		return s.Pair.Remote
	}
	return s.Pair.Remote + "/" + rel
}

// relOf maps a server path into this pair ("", false if outside it).
func (s *Syncer) relOf(remote string) (string, bool) {
	if s.Pair.Remote == "" {
		return remote, remote != ""
	}
	if rel, ok := strings.CutPrefix(remote, s.Pair.Remote+"/"); ok {
		return rel, true
	}
	return "", false
}

func (s *Syncer) abs(rel string) string { return filepath.Join(s.Pair.Local, filepath.FromSlash(rel)) }

// Ignored names are never synced.
func Ignored(name string) bool {
	switch {
	case name == ".DS_Store", name == "Thumbs.db", name == "desktop.ini", name == ".localized":
		return true
	case strings.HasPrefix(name, ".pdrive"), strings.HasPrefix(name, "~$"), strings.HasPrefix(name, ".~lock."):
		return true
	case strings.HasSuffix(name, ".tmp"), strings.HasSuffix(name, ".swp"), strings.HasSuffix(name, ".crdownload"), strings.HasSuffix(name, ".part"):
		return true
	}
	for _, r := range name {
		if r < 32 {
			return true
		}
	}
	return false
}

// ---- snapshots -------------------------------------------------------------------

type localInfo struct {
	IsDir   bool
	Size    int64
	MTimeNs int64
}

func (s *Syncer) scanLocal() (map[string]localInfo, error) {
	out := map[string]localInfo{}
	err := filepath.WalkDir(s.Pair.Local, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == s.Pair.Local {
				return err
			}
			return nil // unreadable entry: skip, report nothing
		}
		if p == s.Pair.Local {
			return nil
		}
		if Ignored(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil // symlinks etc.
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.Pair.Local, p)
		out[filepath.ToSlash(rel)] = localInfo{IsDir: d.IsDir(), Size: info.Size(), MTimeNs: info.ModTime().UnixNano()}
		return nil
	})
	return out, err
}

// remoteTree lists everything under the pair's remote folder.
func (s *Syncer) remoteTree(ctx context.Context) (map[string]*client.File, error) {
	out := map[string]*client.File{}
	queue := []string{s.Pair.Remote}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		items, err := s.C.List(ctx, s.Pair.SpaceID, dir)
		if err != nil {
			if dir == s.Pair.Remote && client.IsStatus(err, 404) {
				return out, nil // folder doesn't exist yet
			}
			return nil, err
		}
		for i := range items {
			f := &items[i]
			rel, ok := s.relOf(f.Path)
			if !ok || Ignored(f.Name) {
				continue
			}
			out[rel] = f
			if f.IsDir {
				queue = append(queue, f.Path)
			}
		}
	}
	return out, nil
}

// remoteLookup returns current server state for specific paths, listing each
// parent directory once.
type remoteLookup struct {
	s     *Syncer
	ctx   context.Context
	dirs  map[string]map[string]*client.File
	known map[string]*client.File // preloaded (first sync)
}

func (l *remoteLookup) get(rel string) (*client.File, error) {
	if l.known != nil {
		return l.known[rel], nil
	}
	parent := parentOf(rel)
	entries, ok := l.dirs[parent]
	if !ok {
		entries = map[string]*client.File{}
		items, err := l.s.C.List(l.ctx, l.s.Pair.SpaceID, l.s.remotePath(parent))
		if err != nil && !client.IsStatus(err, 404) && !client.IsStatus(err, 409) {
			return nil, err
		}
		for i := range items {
			entries[items[i].Name] = &items[i]
		}
		l.dirs[parent] = entries
	}
	return entries[path.Base(rel)], nil
}

// ---- the sync cycle ------------------------------------------------------------------

// SyncOnce runs one full reconciliation.
func (s *Syncer) SyncOnce(ctx context.Context) (*Report, error) {
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Pair.Local, 0o755); err != nil {
		return nil, err
	}
	if s.Settle == 0 {
		s.Settle = 2 * time.Second
	}
	rep := &Report{}

	// Remote side: what changed since our cursor (or everything, first time).
	look := &remoteLookup{s: s, ctx: ctx, dirs: map[string]map[string]*client.File{}}
	touched := map[string]bool{}
	var newCursor int64
	if !s.st.Initialized {
		cur, err := s.C.Cursor(ctx) // before listing, so nothing is missed
		if err != nil {
			return nil, err
		}
		tree, err := s.remoteTree(ctx)
		if err != nil {
			return nil, err
		}
		look.known = tree
		for rel := range tree {
			touched[rel] = true
		}
		newCursor = cur
	} else {
		cur := s.st.Cursor
		for {
			changes, next, more, err := s.C.Changes(ctx, cur, 0)
			if err != nil {
				return nil, err
			}
			for _, c := range changes {
				if (c.SpaceID == nil && s.Pair.SpaceID != 0) || (c.SpaceID != nil && *c.SpaceID != s.Pair.SpaceID) {
					continue
				}
				for _, p := range []string{c.Path, c.OldPath} {
					if rel, ok := s.relOf(p); ok && p != "" {
						touched[rel] = true
						// A directory moved/trashed/restored: everything under it.
						if b := s.st.Entries[rel]; b != nil && b.IsDir || c.File != nil && c.File.IsDir {
							for r := range s.st.Entries {
								if strings.HasPrefix(r, rel+"/") {
									touched[r] = true
								}
							}
							if c.File != nil && c.File.IsDir && c.File.State == "active" && p == c.Path {
								sub := &Syncer{C: s.C, Pair: Pair{Local: s.Pair.Local, Remote: p, SpaceID: s.Pair.SpaceID}}
								if tree, err := sub.remoteTree(ctx); err == nil {
									for r := range tree {
										touched[rel+"/"+r] = true
									}
								}
							}
						}
					}
				}
			}
			cur = next
			if !more {
				break
			}
		}
		newCursor = cur
	}

	// Local side: everything that differs from the base.
	local, err := s.scanLocal()
	if err != nil {
		return nil, err
	}
	for rel, l := range local {
		b := s.st.Entries[rel]
		if b == nil || b.IsDir != l.IsDir || (!l.IsDir && (b.Size != l.Size || b.MTimeNs != l.MTimeNs)) {
			touched[rel] = true
		}
	}
	for rel := range s.st.Entries {
		if _, ok := local[rel]; !ok {
			touched[rel] = true
		}
	}

	// Creations/updates parents-first, then deletions children-first.
	paths := make([]string, 0, len(touched))
	for p := range touched {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var deletions []string
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		del, err := s.reconcile(ctx, rel, local, look, rep)
		if err != nil {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", rel, err))
			if client.Retryable(err) && isNetwork(err) {
				s.save()
				return rep, err // server unreachable: stop the cycle, keep the cursor
			}
		}
		if del {
			deletions = append(deletions, rel)
		}
		if err := s.save(); err != nil {
			return rep, err
		}
	}
	for i := len(deletions) - 1; i >= 0; i-- {
		if err := s.applyDeletion(ctx, deletions[i], local, look, rep); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", deletions[i], err))
		}
	}

	// Only advance the cursor once every remote change was handled.
	if len(rep.Errors) == 0 {
		s.st.Cursor = newCursor
		s.st.Initialized = true
	}
	return rep, s.save()
}

func isNetwork(err error) bool {
	var e *client.Error
	return errors.As(err, &e) && (e.Status == 0 || e.Status == 503)
}

// reconcile handles one path. It returns true if the path needs a deletion,
// which is applied later (deepest first).
func (s *Syncer) reconcile(ctx context.Context, rel string, local map[string]localInfo, look *remoteLookup, rep *Report) (bool, error) {
	b := s.st.Entries[rel]
	l, hasLocal := local[rel]
	r, err := look.get(rel)
	if err != nil {
		return false, err
	}

	// Still being written: try next cycle.
	if hasLocal && !l.IsDir && time.Since(time.Unix(0, l.MTimeNs)) < s.Settle {
		rep.Skipped++
		return false, nil
	}

	localChanged := (b == nil) != !hasLocal || (b != nil && hasLocal &&
		(b.IsDir != l.IsDir || (!l.IsDir && (b.Size != l.Size || b.MTimeNs != l.MTimeNs))))
	remoteChanged := (b == nil) != (r == nil) || (b != nil && r != nil && (b.FileID != r.ID || b.Version != r.Version || b.IsDir != r.IsDir))

	// A touched mtime with identical content isn't a change.
	var localSHA string
	if localChanged && hasLocal && !l.IsDir && b != nil && !b.IsDir && b.Size == l.Size {
		if localSHA, err = hashFile(s.abs(rel)); err != nil {
			return false, err
		}
		if localSHA == b.SHA256 {
			b.MTimeNs = l.MTimeNs
			localChanged = false
		}
	}

	switch {
	case !localChanged && !remoteChanged:
		return false, nil

	case localChanged && !remoteChanged:
		if !hasLocal {
			return r != nil, nil // deleted locally → delete on server (later)
		}
		return false, s.push(ctx, rel, l, b, localSHA, local, look, rep)

	case !localChanged && remoteChanged:
		if r == nil {
			return hasLocal, nil // deleted on server → remove locally (later)
		}
		return false, s.pull(ctx, rel, r, local, false, rep)

	default: // both changed
		switch {
		case !hasLocal && r == nil:
			delete(s.st.Entries, rel)
			return false, nil
		case !hasLocal:
			return false, s.pull(ctx, rel, r, local, false, rep) // deleted here, edited there: keep the edit
		case r == nil:
			return false, s.push(ctx, rel, l, nil, localSHA, local, look, rep) // edited here, deleted there: keep the edit
		case l.IsDir && r.IsDir:
			s.st.Entries[rel] = &Entry{IsDir: true, FileID: r.ID, Version: r.Version}
			return false, nil
		case l.IsDir != r.IsDir:
			return false, fmt.Errorf("a file and a folder have the same name here and on the server; rename one")
		}
		if localSHA == "" {
			if localSHA, err = hashFile(s.abs(rel)); err != nil {
				return false, err
			}
		}
		if localSHA == r.SHA256 {
			s.st.Entries[rel] = entryFor(r, l)
			return false, nil
		}
		// Genuine conflict: push ours. The server keeps both because our base
		// is stale, and push then brings the server's version here.
		return false, s.push(ctx, rel, l, b, localSHA, local, look, rep)
	}
}

func parentOf(rel string) string {
	if p := path.Dir(rel); p != "." {
		return p
	}
	return ""
}

func entryFor(f *client.File, l localInfo) *Entry {
	return &Entry{IsDir: f.IsDir, FileID: f.ID, Version: f.Version, SHA256: f.SHA256, Size: l.Size, MTimeNs: l.MTimeNs}
}

// push uploads a local file or creates a folder on the server. On a conflict
// the local edit is saved on the server under a conflict name, and the
// server's version is then brought here so both sides settle in one cycle.
func (s *Syncer) push(ctx context.Context, rel string, l localInfo, b *Entry, sha string, local map[string]localInfo, look *remoteLookup, rep *Report) error {
	if l.IsDir {
		f, err := s.C.Mkdir(ctx, s.Pair.SpaceID, s.remotePath(rel))
		if err != nil {
			return err
		}
		s.st.Entries[rel] = &Entry{IsDir: true, FileID: f.ID, Version: f.Version}
		return nil
	}
	var err error
	if sha == "" {
		if sha, err = hashFile(s.abs(rel)); err != nil {
			return err
		}
	}
	// Re-stat: the file may have changed since the scan.
	st, err := os.Stat(s.abs(rel))
	if err != nil {
		return err
	}
	if st.ModTime().UnixNano() != l.MTimeNs || st.Size() != l.Size {
		rep.Skipped++
		return nil
	}
	var base int64
	if b != nil && !b.IsDir {
		base = b.Version
	}
	res, err := s.C.Upload(ctx, s.Pair.SpaceID, s.remotePath(rel), s.abs(rel), sha, base)
	if err != nil {
		return err
	}
	if res.Conflict {
		rep.Conflicts++
		s.Log.Warn("conflict: both sides changed; kept both", "path", rel, "copy", res.File.Path)
		delete(look.dirs, parentOf(rel))
		look.known = nil
		cur, err := look.get(rel)
		if err != nil || cur == nil {
			return err
		}
		return s.pull(ctx, rel, cur, local, true, rep)
	}
	rep.Uploaded++
	s.st.Entries[rel] = &Entry{FileID: res.File.ID, Version: res.File.Version, SHA256: sha, Size: l.Size, MTimeNs: l.MTimeNs}
	return nil
}

// pull downloads a server file (or creates a folder) at rel, replacing what's
// there. localSaved says the current local content is already on the server.
func (s *Syncer) pull(ctx context.Context, rel string, r *client.File, local map[string]localInfo, localSaved bool, rep *Report) error {
	dst := s.abs(rel)
	if r.IsDir {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		s.st.Entries[rel] = &Entry{IsDir: true, FileID: r.ID, Version: r.Version}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Never overwrite unsynced local content: if what's on disk now differs
	// from both the base and the server copy, set it aside first.
	if st, err := os.Stat(dst); err == nil && !st.IsDir() && !localSaved {
		b := s.st.Entries[rel]
		if b == nil || st.Size() != b.Size || st.ModTime().UnixNano() != b.MTimeNs {
			if sha, err := hashFile(dst); err == nil && sha != r.SHA256 && (b == nil || sha != b.SHA256) {
				if err := s.toTrash(rel); err != nil {
					return err
				}
			}
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".pdrive-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	// A moved/copied file we already have locally doesn't need downloading.
	if src := s.localCopyOf(r.SHA256, local); src != "" {
		if f, err := os.Open(src); err == nil {
			_, err = io.Copy(io.MultiWriter(tmp, h), f)
			f.Close()
			if err != nil {
				tmp.Close()
				return err
			}
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		tmp.Truncate(0)
		tmp.Seek(0, 0)
		h.Reset()
		if err := s.C.Download(ctx, r.ID, io.MultiWriter(tmp, h)); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != r.SHA256 {
		return fmt.Errorf("downloaded content hash mismatch")
	}
	os.Chtimes(tmp.Name(), r.MTime, r.MTime)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return err
	}
	s.st.Entries[rel] = &Entry{FileID: r.ID, Version: r.Version, SHA256: r.SHA256, Size: st.Size(), MTimeNs: st.ModTime().UnixNano()}
	rep.Downloaded++
	return nil
}

func (s *Syncer) localCopyOf(sha string, local map[string]localInfo) string {
	for rel, e := range s.st.Entries {
		if e.SHA256 != sha || e.IsDir {
			continue
		}
		if l, ok := local[rel]; ok && l.Size == e.Size && l.MTimeNs == e.MTimeNs {
			return s.abs(rel)
		}
	}
	return ""
}

// applyDeletion propagates a one-sided deletion.
func (s *Syncer) applyDeletion(ctx context.Context, rel string, local map[string]localInfo, look *remoteLookup, rep *Report) error {
	b := s.st.Entries[rel]
	if _, hasLocal := local[rel]; !hasLocal {
		// Deleted locally → trash on the server (directories only once empty,
		// so files added on the server meanwhile aren't swept away).
		r, err := look.get(rel)
		if err != nil {
			return err
		}
		if r != nil {
			if r.IsDir {
				items, err := s.C.List(ctx, s.Pair.SpaceID, s.remotePath(rel))
				if err != nil && !client.IsStatus(err, 404) {
					return err
				}
				if len(items) > 0 {
					return nil // still has content the other side didn't delete
				}
			}
			if err := s.C.Trash(ctx, r.ID); err != nil && !client.IsStatus(err, 404) {
				return err
			}
			rep.DeletedRemote++
		}
		delete(s.st.Entries, rel)
		return s.save()
	}
	// Deleted on the server → move local copy to the sync trash.
	if b != nil && b.IsDir {
		entries, _ := os.ReadDir(s.abs(rel))
		if len(entries) > 0 {
			return nil // local content the server never had: keep
		}
		os.Remove(s.abs(rel))
	} else if err := s.toTrash(rel); err != nil {
		return err
	}
	rep.DeletedLocal++
	delete(s.st.Entries, rel)
	return s.save()
}

func (s *Syncer) toTrash(rel string) error {
	dst := filepath.Join(s.TrashDir(), time.Now().Format("2006-01-02"), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		dst += time.Now().Format(".150405.000000")
	}
	if err := os.Rename(s.abs(rel), dst); err != nil {
		// Different filesystem: copy then remove.
		if err := copyFile(s.abs(rel), dst); err != nil {
			return err
		}
		return os.Remove(s.abs(rel))
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
