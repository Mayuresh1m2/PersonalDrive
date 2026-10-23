// Package library is the core file service. It keeps the on-disk library
// (storage) and the index (metadata) consistent, enforces permissions, handles
// conflicts and dedup, and publishes change events. The API and the CLI both
// go through it.
package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/events"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

var (
	ErrForbidden    = errors.New("forbidden")
	ErrNotFound     = metadata.ErrNotFound
	ErrIsDir        = errors.New("path is a directory")
	ErrNotDir       = errors.New("a parent of this path is a file")
	ErrExists       = errors.New("destination already exists")
	ErrHashMismatch = errors.New("content does not match the declared hash/size")
	ErrQuota        = errors.New("storage quota exceeded")
	ErrDirNotEmpty  = errors.New("directory is not empty")
)

func init() {
	for ext, typ := range map[string]string{
		".heic": "image/heic", ".heif": "image/heif", ".avif": "image/avif", ".webp": "image/webp",
		".dng": "image/x-adobe-dng", ".cr2": "image/x-canon-cr2", ".cr3": "image/x-canon-cr3",
		".nef": "image/x-nikon-nef", ".arw": "image/x-sony-arw", ".mov": "video/quicktime",
		".mp4": "video/mp4", ".m4v": "video/x-m4v", ".mkv": "video/x-matroska", ".3gp": "video/3gpp",
	} {
		mime.AddExtensionType(ext, typ)
	}
}

// DedupScope controls which existing files count for "already have it".
type DedupScope string

const (
	ScopeHousehold DedupScope = "household"
	ScopeUser      DedupScope = "user"
)

type Options struct {
	DedupScope     DedupScope
	TrashRetention time.Duration
}

type Service struct {
	db    *metadata.DB
	fs    *storage.LocalStore
	bus   *events.Bus
	guard *volume.Guard
	opts  Options
	log   *slog.Logger

	// mu serialises mutations so disk and index stay in step.
	mu sync.Mutex
}

func New(db *metadata.DB, fs *storage.LocalStore, bus *events.Bus, guard *volume.Guard, opts Options, log *slog.Logger) *Service {
	return &Service{db: db, fs: fs, bus: bus, guard: guard, opts: opts, log: log}
}

func (s *Service) DB() *metadata.DB           { return s.db }
func (s *Service) Store() *storage.LocalStore { return s.fs }
func (s *Service) Guard() *volume.Guard       { return s.guard }

// ---- Owners and permissions ---------------------------------------------------

// Access is a resolved owner plus what the caller may do with it.
type Access struct {
	Owner metadata.Owner
	Ref   storage.OwnerRef
	Write bool
}

// UserAccess returns access to a user's own library.
func (s *Service) UserAccess(u *metadata.User) Access {
	return Access{Owner: metadata.Owner{Kind: "user", ID: u.ID}, Ref: storage.OwnerRef{Kind: "user", Name: u.Username}, Write: true}
}

// Resolve returns the caller's access to their library (spaceID == 0) or a space.
func (s *Service) Resolve(ctx context.Context, u *metadata.User, spaceID int64) (Access, error) {
	if spaceID == 0 {
		return s.UserAccess(u), nil
	}
	sp, err := s.db.SpaceByID(ctx, spaceID)
	if err != nil {
		return Access{}, err
	}
	perm, err := s.db.SpacePermission(ctx, spaceID, u.ID)
	if err != nil {
		return Access{}, err
	}
	if perm == "" {
		return Access{}, ErrNotFound // don't reveal spaces the user isn't in
	}
	return Access{
		Owner: metadata.Owner{Kind: "space", ID: sp.ID},
		Ref:   storage.OwnerRef{Kind: "space", Name: sp.Name},
		Write: perm == "write",
	}, nil
}

// SpaceAccess returns full access to a space (admin tools, not end users).
func (s *Service) SpaceAccess(ctx context.Context, spaceID int64) (Access, error) {
	sp, err := s.db.SpaceByID(ctx, spaceID)
	if err != nil {
		return Access{}, err
	}
	return Access{
		Owner: metadata.Owner{Kind: "space", ID: sp.ID},
		Ref:   storage.OwnerRef{Kind: "space", Name: sp.Name},
		Write: true,
	}, nil
}

// AccessForOwner resolves access for an owner found on a file row.
func (s *Service) AccessForOwner(ctx context.Context, u *metadata.User, o metadata.Owner) (Access, error) {
	if o.Kind == "user" {
		if o.ID != u.ID {
			return Access{}, ErrNotFound
		}
		return s.UserAccess(u), nil
	}
	return s.Resolve(ctx, u, o.ID)
}

// ownerRef builds the on-disk owner reference for an index owner.
func (s *Service) ownerRef(ctx context.Context, o metadata.Owner) (storage.OwnerRef, error) {
	if o.Kind == "user" {
		u, err := s.db.UserByID(ctx, o.ID)
		if err != nil {
			return storage.OwnerRef{}, err
		}
		return storage.OwnerRef{Kind: "user", Name: u.Username}, nil
	}
	sp, err := s.db.SpaceByID(ctx, o.ID)
	if err != nil {
		return storage.OwnerRef{}, err
	}
	return storage.OwnerRef{Kind: "space", Name: sp.Name}, nil
}

// FileForUser loads a file and checks the user can see it.
func (s *Service) FileForUser(ctx context.Context, u *metadata.User, id int64) (*metadata.File, Access, error) {
	f, err := s.db.FileByID(ctx, nil, id)
	if err != nil {
		return nil, Access{}, err
	}
	a, err := s.AccessForOwner(ctx, u, f.Owner)
	if err != nil {
		return nil, Access{}, err
	}
	return f, a, nil
}

func (s *Service) writable(a Access) error {
	if !a.Write {
		return ErrForbidden
	}
	return s.guard.Writable()
}

// ---- Reads ----------------------------------------------------------------------

func (s *Service) List(ctx context.Context, a Access, dir, after string, limit int) ([]*metadata.File, error) {
	dir, err := storage.CleanPath(dir)
	if err != nil {
		return nil, err
	}
	if dir != "" {
		d, err := s.db.ActiveFileByPath(ctx, nil, a.Owner, dir)
		if err != nil {
			return nil, err
		}
		if !d.IsDir {
			return nil, ErrNotDir
		}
	}
	return s.db.ListChildren(ctx, a.Owner, dir, after, limit)
}

// Open returns the file's content.
func (s *Service) Open(ctx context.Context, a Access, f *metadata.File) (*os.File, error) {
	if f.IsDir {
		return nil, ErrIsDir
	}
	if f.State != "active" {
		return nil, ErrNotFound
	}
	return s.fs.Open(ctx, a.Ref, f.Path)
}

// AbsPath returns where an active file lives on disk (for processors).
func (s *Service) AbsPath(ctx context.Context, f *metadata.File) (string, error) {
	ref, err := s.ownerRef(ctx, f.Owner)
	if err != nil {
		return "", err
	}
	return s.fs.Abs(ref, f.Path), nil
}

// ---- Writes ---------------------------------------------------------------------

// CommitRequest describes new content waiting in a temp file.
type CommitRequest struct {
	Access      Access
	Path        string
	TempPath    string // file in the library .tmp dir; consumed on success
	SHA256      string // verified hash of TempPath
	Size        int64
	MTime       time.Time
	BaseVersion *int64 // version the client last saw; nil = new file
	DeviceID    string
	DeviceName  string
}

type CommitResult struct {
	File      *metadata.File
	Conflict  bool             // stored under a conflict name
	Unchanged bool             // identical content was already there
	Linked    storage.LinkMode // non-empty if on-disk dedup was used
}

// Commit places new content at a path, applying conflict and dedup rules.
func (s *Service) Commit(ctx context.Context, req CommitRequest) (*CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(ctx, req, "")
}

// LinkExisting adds a file whose content the server already holds, without
// any bytes being uploaded. It returns ErrNotFound if no copy exists.
func (s *Service) LinkExisting(ctx context.Context, req CommitRequest) (*CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.findContentOnDisk(ctx, nil, req.SHA256, req.Size)
	if err != nil {
		return nil, err
	}
	return s.commitLocked(ctx, req, src)
}

// commitLocked places either req.TempPath or a link to linkSrc.
func (s *Service) commitLocked(ctx context.Context, req CommitRequest, linkSrc string) (res *CommitResult, err error) {
	if req.TempPath != "" {
		defer func() {
			if err != nil {
				os.Remove(req.TempPath)
			}
		}()
	}
	if err := s.writable(req.Access); err != nil {
		return nil, err
	}
	p, err := storage.CleanPath(req.Path)
	if err != nil {
		return nil, err
	}
	if p == "" {
		return nil, fmt.Errorf("%w: empty path", storage.ErrInvalidPath)
	}
	if req.Access.Owner.Kind == "user" {
		if err := s.checkQuota(ctx, req.Access.Owner.ID, req.Size); err != nil {
			return nil, err
		}
	}
	if err := s.checkParents(ctx, req.Access.Owner, p); err != nil {
		return nil, err
	}

	res = &CommitResult{}
	existing, err := s.db.ActiveFileByPath(ctx, nil, req.Access.Owner, p)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	replace := false
	if existing != nil {
		switch {
		case existing.IsDir:
			return nil, ErrIsDir
		case existing.SHA256 == req.SHA256 && existing.Size == req.Size:
			res.File, res.Unchanged = existing, true
			if req.TempPath != "" {
				os.Remove(req.TempPath)
			}
			return res, nil
		case req.BaseVersion != nil && *req.BaseVersion == existing.Version:
			replace = true
		default:
			p, err = s.freeConflictName(ctx, req.Access.Owner, p, req.DeviceName)
			if err != nil {
				return nil, err
			}
			res.Conflict = true
		}
	}

	// On-disk dedup: link to an existing copy instead of keeping the upload.
	src := linkSrc
	if src == "" && s.fs.LinkMode() != storage.LinkOff {
		src, _ = s.findContentOnDisk(ctx, nil, req.SHA256, req.Size)
	}
	tmp, mtime := req.TempPath, req.MTime
	if src != "" {
		linked, used, err := s.fs.LinkInto(src)
		if err != nil {
			return nil, err
		}
		if req.TempPath != "" {
			os.Remove(req.TempPath)
		}
		tmp = linked
		res.Linked = used
		if used == storage.LinkHardlink {
			mtime = time.Time{} // a hardlink shares its inode's mtime; the index keeps ours
		}
	}

	// Ensure index rows exist for parent directories before touching disk.
	if err := s.ensureDirs(ctx, req.Access, metadata.ParentOf(p), req.DeviceID); err != nil {
		os.Remove(tmp)
		return nil, err
	}

	var trashRel string
	if replace {
		if trashRel, err = s.fs.MoveToTrash(req.Access.Ref, p); err != nil {
			os.Remove(tmp)
			return nil, err
		}
	}
	if err := s.fs.Place(tmp, req.Access.Ref, p, mtime, false); err != nil {
		os.Remove(tmp)
		if trashRel != "" {
			s.fs.RestoreFromTrash(trashRel, req.Access.Ref, p)
		}
		if errors.Is(err, storage.ErrExists) {
			return nil, fmt.Errorf("%w: file exists on disk but not in the index (run pdrive reindex)", ErrExists)
		}
		return nil, err
	}

	f := &metadata.File{
		Owner: req.Access.Owner, Path: p, Size: req.Size, MTime: req.MTime.UTC(),
		SHA256: req.SHA256, Mime: detectMime(p), DeviceID: req.DeviceID,
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		if replace {
			old := *existing
			old.ID, old.State, old.TrashPath, old.TrashReason = 0, "trashed", trashRel, "replaced"
			t := time.Now().UTC()
			old.TrashedAt = &t
			if err := s.db.InsertFile(ctx, tx, &old); err != nil {
				return err
			}
			if err := s.db.SetOwnTrashRoot(ctx, tx, old.ID); err != nil {
				return err
			}
			f.ID, f.Version, f.CreatedAt = existing.ID, existing.Version+1, existing.CreatedAt
			if err := s.db.UpdateContent(ctx, tx, f); err != nil {
				return err
			}
			_, err := s.db.AddChange(ctx, tx, f, "update", "")
			return err
		}
		if err := s.db.InsertFile(ctx, tx, f); err != nil {
			return err
		}
		_, err := s.db.AddChange(ctx, tx, f, "create", "")
		return err
	})
	if err != nil {
		// Undo the disk change so disk and index agree.
		if trashRel2, terr := s.fs.MoveToTrash(req.Access.Ref, p); terr == nil {
			s.fs.PurgeTrash(trashRel2)
		}
		if trashRel != "" {
			s.fs.RestoreFromTrash(trashRel, req.Access.Ref, p)
		}
		return nil, err
	}
	topic := events.FileCreated
	if replace {
		topic = events.FileUpdated
	}
	s.publish(ctx, topic, f)
	res.File = f
	return res, nil
}

// Mkdir creates a directory (and parents).
func (s *Service) Mkdir(ctx context.Context, a Access, dir, deviceID string) (*metadata.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(a); err != nil {
		return nil, err
	}
	p, err := storage.CleanPath(dir)
	if err != nil {
		return nil, err
	}
	if p == "" {
		return nil, fmt.Errorf("%w: empty path", storage.ErrInvalidPath)
	}
	if err := s.checkParents(ctx, a.Owner, p); err != nil {
		return nil, err
	}
	if err := s.ensureDirs(ctx, a, p, deviceID); err != nil {
		return nil, err
	}
	f, err := s.db.ActiveFileByPath(ctx, nil, a.Owner, p)
	if err != nil {
		return nil, err
	}
	if !f.IsDir {
		return nil, ErrExists
	}
	return f, nil
}

// ensureDirs creates directory rows (and directories on disk) for dir and
// every ancestor that doesn't exist yet. Caller holds s.mu.
func (s *Service) ensureDirs(ctx context.Context, a Access, dir, deviceID string) error {
	if dir == "" {
		return nil
	}
	var missing []string
	for d := dir; d != ""; d = metadata.ParentOf(d) {
		f, err := s.db.ActiveFileByPath(ctx, nil, a.Owner, d)
		if errors.Is(err, ErrNotFound) {
			missing = append(missing, d)
			continue
		}
		if err != nil {
			return err
		}
		if !f.IsDir {
			return ErrNotDir
		}
		break
	}
	if len(missing) == 0 {
		return nil
	}
	if err := s.fs.Mkdir(a.Ref, dir); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		st, err := s.fs.Stat(ctx, a.Ref, d)
		if err != nil {
			return err
		}
		f := &metadata.File{Owner: a.Owner, Path: d, IsDir: true, MTime: st.ModTime.UTC(), DeviceID: deviceID}
		err = s.db.Tx(ctx, func(tx *sql.Tx) error {
			if err := s.db.InsertFile(ctx, tx, f); err != nil {
				return err
			}
			_, err := s.db.AddChange(ctx, tx, f, "create", "")
			return err
		})
		if err != nil {
			return err
		}
		s.publish(ctx, events.FileCreated, f)
	}
	return nil
}

// Move renames a file or directory within the same owner.
func (s *Service) Move(ctx context.Context, a Access, f *metadata.File, newPath, deviceID string) (*metadata.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(a); err != nil {
		return nil, err
	}
	np, err := storage.CleanPath(newPath)
	if err != nil {
		return nil, err
	}
	if np == "" {
		return nil, fmt.Errorf("%w: empty path", storage.ErrInvalidPath)
	}
	if np == f.Path {
		return f, nil
	}
	if f.IsDir && strings.HasPrefix(np+"/", f.Path+"/") {
		return nil, fmt.Errorf("%w: cannot move a directory into itself", storage.ErrInvalidPath)
	}
	if _, err := s.db.ActiveFileByPath(ctx, nil, a.Owner, np); err == nil {
		return nil, ErrExists
	}
	if err := s.checkParents(ctx, a.Owner, np); err != nil {
		return nil, err
	}
	if err := s.ensureDirs(ctx, a, metadata.ParentOf(np), deviceID); err != nil {
		return nil, err
	}
	if err := s.fs.Move(ctx, a.Ref, f.Path, np); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, ErrExists
		}
		return nil, err
	}
	old := f.Path
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.db.MoveTree(ctx, tx, a.Owner, old, np); err != nil {
			return err
		}
		f.Path, f.Parent, f.Name = np, metadata.ParentOf(np), path.Base(np)
		_, err := s.db.AddChange(ctx, tx, f, "move", old)
		return err
	})
	if err != nil {
		s.fs.Move(ctx, a.Ref, np, old)
		return nil, err
	}
	s.publish(ctx, events.FileMoved, f)
	return f, nil
}

// Trash moves a file or directory (with contents) to the trash.
func (s *Service) Trash(ctx context.Context, a Access, f *metadata.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(a); err != nil {
		return err
	}
	if f.State != "active" {
		return ErrNotFound
	}
	trashRel, err := s.fs.MoveToTrash(a.Ref, f.Path)
	if err != nil {
		return err
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.db.TrashTree(ctx, tx, f, trashRel, "deleted"); err != nil {
			return err
		}
		_, err := s.db.AddChange(ctx, tx, f, "trash", "")
		return err
	})
	if err != nil {
		s.fs.RestoreFromTrash(trashRel, a.Ref, f.Path)
		return err
	}
	s.publish(ctx, events.FileTrashed, f)
	return nil
}

// ListTrash returns trash entries (deleted items and replaced versions).
func (s *Service) ListTrash(ctx context.Context, a Access) ([]*metadata.File, error) {
	return s.db.TrashedRoots(ctx, a.Owner)
}

// Restore brings a trash entry back. If the original path is taken, a
// "(restored)" suffix is added.
func (s *Service) Restore(ctx context.Context, a Access, f *metadata.File, deviceID string) (*metadata.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(a); err != nil {
		return nil, err
	}
	if f.State != "trashed" || f.TrashRoot != f.ID {
		return nil, ErrNotFound
	}
	target := f.Path
	if _, err := s.db.ActiveFileByPath(ctx, nil, a.Owner, target); err == nil {
		target, err = s.freeSuffixName(ctx, a.Owner, target, "restored")
		if err != nil {
			return nil, err
		}
	}
	if err := s.checkParents(ctx, a.Owner, target); err != nil {
		return nil, err
	}
	if err := s.ensureDirs(ctx, a, metadata.ParentOf(target), deviceID); err != nil {
		return nil, err
	}
	if err := s.fs.RestoreFromTrash(f.TrashPath, a.Ref, target); err != nil {
		return nil, err
	}
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.db.RestoreTree(ctx, tx, f, target); err != nil {
			return err
		}
		f.Path, f.Parent, f.Name, f.State = target, metadata.ParentOf(target), path.Base(target), "active"
		_, err := s.db.AddChange(ctx, tx, f, "restore", "")
		return err
	})
	if err != nil {
		return nil, err
	}
	s.publish(ctx, events.FileRestore, f)
	return f, nil
}

// PurgeTrash permanently deletes one trash entry.
func (s *Service) PurgeTrash(ctx context.Context, f *metadata.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard.Writable(); err != nil {
		return err
	}
	if err := s.fs.PurgeTrash(f.TrashPath); err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx *sql.Tx) error { return s.db.DeleteTrashTree(ctx, tx, f.ID) })
}

// PurgeExpiredTrash deletes trash entries older than the retention period.
func (s *Service) PurgeExpiredTrash(ctx context.Context) (int, error) {
	roots, err := s.db.ExpiredTrashRoots(ctx, time.Now().Add(-s.opts.TrashRetention))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range roots {
		if err := s.PurgeTrash(ctx, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---- Dedup lookups ----------------------------------------------------------------

// FindForUpload returns true if the caller may skip uploading content with
// this hash, honouring the dedup scope.
func (s *Service) FindForUpload(ctx context.Context, u *metadata.User, sha string, size int64) (bool, error) {
	var owners []metadata.Owner
	if s.opts.DedupScope == ScopeUser {
		var err error
		if owners, err = s.db.VisibleOwners(ctx, u.ID); err != nil {
			return false, err
		}
	}
	have, err := s.db.HaveContent(ctx, []string{sha}, owners)
	if err != nil || !have[sha] {
		return false, err
	}
	// Check size too: a hash match with a different size is impossible for
	// SHA-256 in practice, but the client asserted both.
	files, err := s.db.AnyActiveByContent(ctx, nil, sha, size, 1)
	return len(files) > 0, err
}

// Have answers "which of these hashes does the server already have?".
func (s *Service) Have(ctx context.Context, u *metadata.User, hashes []string) (map[string]bool, error) {
	var owners []metadata.Owner
	if s.opts.DedupScope == ScopeUser {
		var err error
		if owners, err = s.db.VisibleOwners(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	return s.db.HaveContent(ctx, hashes, owners)
}

// findContentOnDisk returns the absolute path of an existing copy of content.
func (s *Service) findContentOnDisk(ctx context.Context, tx *sql.Tx, sha string, size int64) (string, error) {
	files, err := s.db.AnyActiveByContent(ctx, tx, sha, size, 5)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		abs, err := s.AbsPath(ctx, f)
		if err != nil {
			continue
		}
		if st, err := os.Stat(abs); err == nil && st.Mode().IsRegular() && st.Size() == size {
			return abs, nil
		}
	}
	return "", ErrNotFound
}

// ---- Helpers ------------------------------------------------------------------------

func (s *Service) checkQuota(ctx context.Context, userID, size int64) error {
	u, err := s.db.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.QuotaBytes <= 0 {
		return nil
	}
	used, err := s.db.UserUsage(ctx, userID)
	if err != nil {
		return err
	}
	if used+size > u.QuotaBytes {
		return ErrQuota
	}
	return nil
}

// checkParents fails if any ancestor of p is a regular file.
func (s *Service) checkParents(ctx context.Context, o metadata.Owner, p string) error {
	for d := metadata.ParentOf(p); d != ""; d = metadata.ParentOf(d) {
		f, err := s.db.ActiveFileByPath(ctx, nil, o, d)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !f.IsDir {
			return ErrNotDir
		}
		return nil // an existing directory's ancestors were validated when it was created
	}
	return nil
}

// freeConflictName returns "name (conflict from <device> <date>).ext", made unique.
func (s *Service) freeConflictName(ctx context.Context, o metadata.Owner, p, device string) (string, error) {
	if device == "" {
		device = "another device"
	}
	label := fmt.Sprintf("conflict from %s %s", sanitizeLabel(device), time.Now().Format("2006-01-02 1504"))
	return s.freeSuffixName(ctx, o, p, label)
}

func (s *Service) freeSuffixName(ctx context.Context, o metadata.Owner, p, label string) (string, error) {
	dir, base := path.Split(p)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 1000; i++ {
		l := label
		if i > 1 {
			l = fmt.Sprintf("%s %d", label, i)
		}
		cand := dir + fmt.Sprintf("%s (%s)%s", stem, l, ext)
		if _, err := s.db.ActiveFileByPath(ctx, nil, o, cand); errors.Is(err, ErrNotFound) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", ErrExists
}

func sanitizeLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 32 {
			return '-'
		}
		return r
	}, s)
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func detectMime(p string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(p))); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	return "application/octet-stream"
}

func (s *Service) publish(ctx context.Context, topic string, f *metadata.File) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(ctx, events.Event{
		Topic: topic, FileID: f.ID, SHA256: f.SHA256, Mime: f.Mime, Seq: f.Seq,
		Owner: fmt.Sprintf("%s:%d", f.Owner.Kind, f.Owner.ID),
	})
}

// CopyToTemp streams r into the library temp dir and returns the path, size
// and hash (used by uploads and imports).
func (s *Service) CopyToTemp(r io.Reader) (string, int64, string, error) {
	if err := s.guard.Writable(); err != nil {
		return "", 0, "", err
	}
	return s.fs.WriteTemp(r)
}
