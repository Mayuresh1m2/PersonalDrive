package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/events"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
)

type ReindexReport struct {
	Scanned       int      `json:"scanned"`
	Added         int      `json:"added"`
	Updated       int      `json:"updated"`
	Removed       int      `json:"removed"`
	HashedBytes   int64    `json:"hashed_bytes"`
	UnknownOwners []string `json:"unknown_owners,omitempty"`
}

// Reindex reconciles the index with what is actually on disk. Disk wins:
// rows for missing files are removed, new files are added, and files whose
// size or mtime changed are re-hashed.
func (s *Service) Reindex(ctx context.Context, progress func(string)) (*ReindexReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard.Writable(); err != nil {
		return nil, err
	}
	if progress == nil {
		progress = func(string) {}
	}
	rep := &ReindexReport{}
	refs, err := s.fs.Owners()
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		owner, err := s.ownerFor(ctx, ref)
		if errors.Is(err, ErrNotFound) {
			rep.UnknownOwners = append(rep.UnknownOwners, ref.String())
			continue
		}
		if err != nil {
			return nil, err
		}
		progress("indexing " + ref.String())
		if err := s.reindexOwner(ctx, owner, ref, rep); err != nil {
			return rep, fmt.Errorf("%s: %w", ref, err)
		}
	}
	return rep, nil
}

func (s *Service) ownerFor(ctx context.Context, ref storage.OwnerRef) (Access, error) {
	if ref.Kind == "user" {
		u, err := s.db.UserByName(ctx, ref.Name)
		if err != nil {
			return Access{}, err
		}
		return s.UserAccess(u), nil
	}
	sp, err := s.db.SpaceByName(ctx, ref.Name)
	if err != nil {
		return Access{}, err
	}
	return Access{Owner: metadata.Owner{Kind: "space", ID: sp.ID}, Ref: ref, Write: true}, nil
}

func (s *Service) reindexOwner(ctx context.Context, a Access, ref storage.OwnerRef, rep *ReindexReport) error {
	onDisk := map[string]storage.FileInfo{}
	err := s.fs.Walk(ctx, ref, func(fi storage.FileInfo) error {
		if _, err := storage.CleanPath(fi.Path); err != nil {
			return nil // names we would never create; leave them alone
		}
		onDisk[fi.Path] = fi
		return nil
	})
	if err != nil {
		return err
	}
	rep.Scanned += len(onDisk)

	var stale []*metadata.File
	err = s.db.ActiveFiles(ctx, a.Owner, func(f *metadata.File) error {
		fi, ok := onDisk[f.Path]
		if !ok || fi.IsDir != f.IsDir {
			stale = append(stale, f)
			return nil
		}
		delete(onDisk, f.Path)
		if f.IsDir || (fi.Size == f.Size && fi.ModTime.UnixMilli() == f.MTime.UnixMilli()) {
			return nil
		}
		sum, n, err := storage.HashFile(s.fs.Abs(ref, f.Path))
		if err != nil {
			return err
		}
		rep.HashedBytes += n
		if sum == f.SHA256 && n == f.Size {
			return nil // e.g. a hardlinked copy whose shared mtime changed
		}
		f.SHA256, f.Size, f.MTime, f.Version = sum, n, fi.ModTime.UTC(), f.Version+1
		f.Mime = detectMime(f.Path)
		err = s.db.Tx(ctx, func(tx *sql.Tx) error {
			if err := s.db.UpdateContent(ctx, tx, f); err != nil {
				return err
			}
			_, err := s.db.AddChange(ctx, tx, f, "update", "")
			return err
		})
		if err == nil {
			rep.Updated++
			s.publish(ctx, events.FileUpdated, f)
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, f := range stale {
		err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := s.db.AddChange(ctx, tx, f, "delete", ""); err != nil {
				return err
			}
			return s.db.DeleteFile(ctx, tx, f.ID)
		})
		if err != nil {
			return err
		}
		rep.Removed++
		s.publish(ctx, events.FileDeleted, f)
	}

	// Insert what's left, parents before children.
	paths := make([]string, 0, len(onDisk))
	for p := range onDisk {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fi := onDisk[p]
		f := &metadata.File{Owner: a.Owner, Path: p, IsDir: fi.IsDir, MTime: fi.ModTime.UTC()}
		if !fi.IsDir {
			sum, n, err := storage.HashFile(s.fs.Abs(ref, p))
			if err != nil {
				return err
			}
			rep.HashedBytes += n
			f.SHA256, f.Size, f.Mime = sum, n, detectMime(p)
		}
		err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			if err := s.db.InsertFile(ctx, tx, f); err != nil {
				return err
			}
			_, err := s.db.AddChange(ctx, tx, f, "create", "")
			return err
		})
		if err != nil {
			return err
		}
		rep.Added++
		s.publish(ctx, events.FileCreated, f)
	}
	return nil
}

type ImportReport struct {
	Files       int      `json:"files"`
	Imported    int      `json:"imported"`
	Unchanged   int      `json:"unchanged"`
	Conflicts   int      `json:"conflicts"`
	Linked      int      `json:"linked"`
	Bytes       int64    `json:"bytes"`
	LinkedBytes int64    `json:"linked_bytes"`
	Errors      []string `json:"errors,omitempty"`
}

// junk files that are never worth importing.
var importSkip = map[string]bool{".DS_Store": true, "Thumbs.db": true, "desktop.ini": true, ".localized": true}

// Import copies a local folder into a library under destPrefix, keeping
// relative paths and mtimes. The source is only read, never modified.
// Existing identical files are skipped; differing ones become conflict copies.
func (s *Service) Import(ctx context.Context, a Access, src, destPrefix string, progress func(string)) (*ImportReport, error) {
	if progress == nil {
		progress = func(string) {}
	}
	prefix, err := storage.CleanPath(destPrefix)
	if err != nil {
		return nil, err
	}
	out := &ImportReport{}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			out.Errors = append(out.Errors, err.Error())
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !d.Type().IsRegular() || importSkip[d.Name()] || strings.HasPrefix(d.Name(), "._") {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		dest := path.Join(prefix, filepath.ToSlash(rel))
		out.Files++
		if err := s.importOne(ctx, a, p, dest, out); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", rel, err))
		}
		if out.Files%100 == 0 {
			progress(fmt.Sprintf("%d files processed", out.Files))
		}
		return nil
	})
	return out, err
}

func (s *Service) importOne(ctx context.Context, a Access, srcPath, dest string, out *ImportReport) error {
	if _, err := storage.CleanPath(dest); err != nil {
		return err
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	tmp, size, sum, err := s.CopyToTemp(f)
	if err != nil {
		return err
	}
	res, err := s.Commit(ctx, CommitRequest{
		Access: a, Path: dest, TempPath: tmp, SHA256: sum, Size: size,
		MTime: st.ModTime(), DeviceName: "import",
	})
	if err != nil {
		return err
	}
	switch {
	case res.Unchanged:
		out.Unchanged++
	case res.Conflict:
		out.Conflicts++
		fallthrough
	default:
		out.Imported++
		out.Bytes += size
	}
	if res.Linked != "" && res.Linked != storage.LinkOff {
		out.Linked++
		out.LinkedBytes += size
	}
	return nil
}

// CleanupTemp removes abandoned temp files older than maxAge.
func (s *Service) CleanupTemp(maxAge time.Duration) error {
	if err := s.guard.Writable(); err != nil {
		return err
	}
	return s.fs.CleanTemp(maxAge)
}
