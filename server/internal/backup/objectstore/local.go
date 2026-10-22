package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

// Local stores objects as files under a directory (e.g. external disk B).
// The directory must carry a backup volume marker, so an unplugged disk is
// reported instead of silently backing up onto the laptop SSD.
type Local struct {
	root string
}

// OpenLocal checks the backup marker and returns the store.
func OpenLocal(root string) (*Local, error) {
	m, err := volume.Read(root)
	if err != nil {
		return nil, fmt.Errorf("backup disk at %s: %w (run `pdrive volume init --role backup %s`)", root, err, root)
	}
	if m.Role != volume.RoleBackup {
		return nil, fmt.Errorf("%s is a %s volume, not a backup volume", root, m.Role)
	}
	return &Local{root: root}, nil
}

// NewLocalUnchecked skips the marker check (tests and restores from a copied folder).
func NewLocalUnchecked(root string) *Local { return &Local{root: root} }

func (l *Local) Describe() string { return "file://" + l.root }

func (l *Local) path(key string) (string, error) {
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return filepath.Join(l.root, "repo", filepath.FromSlash(key)), nil
}

func (l *Local) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, _ PutOptions) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".tmp-" + uuid.NewString()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if err == nil && n != size {
		err = fmt.Errorf("short write: %d of %d bytes", n, size)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func (l *Local) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	if length < 0 {
		return f, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, length), f}, nil
}

func (l *Local) Head(ctx context.Context, key string) (ObjectInfo, error) {
	p, err := l.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: st.Size(), LastModified: st.ModTime(), StorageClass: "LOCAL", Restore: RestoreNotNeeded}, nil
}

func (l *Local) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	base := filepath.Join(l.root, "repo")
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.Contains(d.Name(), ".tmp-") {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(ObjectInfo{Key: key, Size: info.Size(), LastModified: info.ModTime(), StorageClass: "LOCAL", Restore: RestoreNotNeeded})
	})
}

func (l *Local) Delete(ctx context.Context, keys ...string) error {
	for _, k := range keys {
		p, err := l.path(k)
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (l *Local) RequestRestore(ctx context.Context, key, tier string, days int) error { return nil }
