// Package storage owns the human-readable on-disk library.
//
// Layout under the library root:
//
//	users/<username>/...   each user's own folder tree
//	shared/<space>/...     shared spaces
//	.trash/<id>/<name>     trashed files and replaced versions
//	.tmp/                  in-flight writes (same filesystem, so renames are atomic)
//
// Files are never modified in place: every write goes to .tmp, is fsynced and
// renamed into position. That invariant is what makes hardlink dedup safe.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// OwnerRef identifies whose tree a path belongs to.
type OwnerRef struct {
	Kind string // "user" or "space"
	Name string // directory name (username or space name)
}

func (o OwnerRef) dir() string {
	if o.Kind == "space" {
		return filepath.Join("shared", o.Name)
	}
	return filepath.Join("users", o.Name)
}

func (o OwnerRef) String() string { return o.Kind + ":" + o.Name }

type FileInfo struct {
	Owner   OwnerRef
	Path    string // relative, '/'-separated
	Size    int64
	ModTime time.Time
	IsDir   bool
	SHA256  string // set only when the operation computed it
}

// FileStore is the contract for primary storage. LocalStore is the only
// implementation; the interface exists so the library service can be tested
// against fakes and so other backends can be added later.
type FileStore interface {
	Put(ctx context.Context, owner OwnerRef, relPath string, r io.Reader, mtime time.Time) (FileInfo, error)
	Open(ctx context.Context, owner OwnerRef, relPath string) (*os.File, error)
	Stat(ctx context.Context, owner OwnerRef, relPath string) (FileInfo, error)
	Move(ctx context.Context, owner OwnerRef, from, to string) error
	Walk(ctx context.Context, owner OwnerRef, fn func(FileInfo) error) error
}

var (
	ErrInvalidPath = errors.New("invalid path")
	ErrExists      = errors.New("destination already exists")
)

const maxPathBytes = 1024

// CleanPath normalises a client-supplied relative path and rejects anything
// that could escape the owner's tree or collide with internal names.
func CleanPath(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrInvalidPath)
	}
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	if len(p) > maxPathBytes {
		return "", fmt.Errorf("%w: too long", ErrInvalidPath)
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		switch {
		case part == "" || part == "." || part == "..":
			return "", fmt.Errorf("%w: empty, '.' or '..' segment", ErrInvalidPath)
		case len(part) > 255:
			return "", fmt.Errorf("%w: segment longer than 255 bytes", ErrInvalidPath)
		case strings.HasPrefix(part, ".pdrive"):
			return "", fmt.Errorf("%w: names starting with .pdrive are reserved", ErrInvalidPath)
		}
		for _, r := range part {
			if unicode.IsControl(r) {
				return "", fmt.Errorf("%w: control character", ErrInvalidPath)
			}
		}
	}
	return strings.Join(parts, "/"), nil
}

// CleanName validates a single path segment (user or space directory names).
func CleanName(name string) (string, error) {
	c, err := CleanPath(name)
	if err != nil {
		return "", err
	}
	if c == "" || strings.Contains(c, "/") || strings.HasPrefix(c, ".") {
		return "", fmt.Errorf("%w: %q is not a valid name", ErrInvalidPath, name)
	}
	return c, nil
}

// LocalStore implements FileStore on a local directory.
type LocalStore struct {
	root     string
	linkMode atomic.Value // LinkMode; re-probed when a disk is (re)attached
}

func NewLocalStore(root string, mode LinkMode) *LocalStore {
	s := &LocalStore{root: root}
	s.linkMode.Store(mode)
	return s
}

func (s *LocalStore) Root() string           { return s.root }
func (s *LocalStore) LinkMode() LinkMode     { return s.linkMode.Load().(LinkMode) }
func (s *LocalStore) SetLinkMode(m LinkMode) { s.linkMode.Store(m) }

// Prepare creates the internal directories. Call only when the volume guard
// reports the library as writable.
func (s *LocalStore) Prepare() error {
	for _, d := range []string{"users", "shared", ".trash", ".tmp"} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o750); err != nil {
			return err
		}
	}
	return nil
}

// Abs returns the absolute path of relPath in owner's tree.
func (s *LocalStore) Abs(owner OwnerRef, relPath string) string {
	return filepath.Join(s.root, owner.dir(), filepath.FromSlash(relPath))
}

// TrashAbs returns the absolute path of a trash-relative path.
func (s *LocalStore) TrashAbs(trashRel string) string {
	return filepath.Join(s.root, ".trash", filepath.FromSlash(trashRel))
}

// CreateTemp creates a file in .tmp for an in-flight write.
func (s *LocalStore) CreateTemp() (*os.File, error) {
	return os.CreateTemp(filepath.Join(s.root, ".tmp"), "w-*")
}

// WriteTemp streams r into a new temp file, fsyncs it and returns its path,
// size and SHA-256. The caller must Place or remove the temp file.
func (s *LocalStore) WriteTemp(r io.Reader) (tmpPath string, size int64, sum string, err error) {
	f, err := s.CreateTemp()
	if err != nil {
		return "", 0, "", err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return "", 0, "", err
	}
	if err = f.Sync(); err != nil {
		return "", 0, "", err
	}
	if err = f.Close(); err != nil {
		return "", 0, "", err
	}
	return f.Name(), size, hex.EncodeToString(h.Sum(nil)), nil
}

// Place atomically moves a temp file to relPath, creating parent directories.
// It fails with ErrExists if the destination exists, unless replace is set.
func (s *LocalStore) Place(tmpPath string, owner OwnerRef, relPath string, mtime time.Time, replace bool) error {
	dst := s.Abs(owner, relPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmpPath, mtime, mtime); err != nil {
			return err
		}
	}
	if !replace {
		if _, err := os.Lstat(dst); err == nil {
			return ErrExists
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// Put writes r to relPath atomically. It refuses to overwrite.
func (s *LocalStore) Put(ctx context.Context, owner OwnerRef, relPath string, r io.Reader, mtime time.Time) (FileInfo, error) {
	tmp, size, sum, err := s.WriteTemp(r)
	if err != nil {
		return FileInfo{}, err
	}
	if err := s.Place(tmp, owner, relPath, mtime, false); err != nil {
		os.Remove(tmp)
		return FileInfo{}, err
	}
	return FileInfo{Owner: owner, Path: relPath, Size: size, ModTime: mtime, SHA256: sum}, nil
}

// LinkInto creates a temp file sharing srcAbs's content using the configured
// link mode (reflink, hardlink, or copy as a fallback) and returns the temp
// path plus the method actually used.
func (s *LocalStore) LinkInto(srcAbs string) (tmpPath string, used LinkMode, err error) {
	tmp := filepath.Join(s.root, ".tmp", "l-"+uuid.NewString())
	switch s.LinkMode() {
	case LinkReflink:
		if err := reflink(srcAbs, tmp); err == nil {
			return tmp, LinkReflink, nil
		}
		os.Remove(tmp)
	case LinkHardlink:
		if err := os.Link(srcAbs, tmp); err == nil {
			return tmp, LinkHardlink, nil
		}
	}
	if err := copyFile(srcAbs, tmp); err != nil {
		os.Remove(tmp)
		return "", LinkOff, err
	}
	return tmp, LinkOff, nil
}

func (s *LocalStore) Open(ctx context.Context, owner OwnerRef, relPath string) (*os.File, error) {
	return os.Open(s.Abs(owner, relPath))
}

func (s *LocalStore) Stat(ctx context.Context, owner OwnerRef, relPath string) (FileInfo, error) {
	st, err := os.Stat(s.Abs(owner, relPath))
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Owner: owner, Path: relPath, Size: st.Size(), ModTime: st.ModTime(), IsDir: st.IsDir()}, nil
}

// Mkdir creates relPath and any missing parents.
func (s *LocalStore) Mkdir(owner OwnerRef, relPath string) error {
	return os.MkdirAll(s.Abs(owner, relPath), 0o750)
}

// Move renames within an owner's tree. The destination must not exist.
func (s *LocalStore) Move(ctx context.Context, owner OwnerRef, from, to string) error {
	src, dst := s.Abs(owner, from), s.Abs(owner, to)
	if _, err := os.Lstat(dst); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// MoveToTrash moves relPath into a fresh trash slot and returns the
// trash-relative path.
func (s *LocalStore) MoveToTrash(owner OwnerRef, relPath string) (string, error) {
	slot := uuid.NewString()
	if err := os.MkdirAll(filepath.Join(s.root, ".trash", slot), 0o750); err != nil {
		return "", err
	}
	trashRel := slot + "/" + path.Base(relPath)
	if err := os.Rename(s.Abs(owner, relPath), s.TrashAbs(trashRel)); err != nil {
		os.Remove(filepath.Join(s.root, ".trash", slot))
		return "", err
	}
	return trashRel, nil
}

// RestoreFromTrash moves a trashed entry back to relPath.
func (s *LocalStore) RestoreFromTrash(trashRel string, owner OwnerRef, relPath string) error {
	dst := s.Abs(owner, relPath)
	if _, err := os.Lstat(dst); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Rename(s.TrashAbs(trashRel), dst); err != nil {
		return err
	}
	os.Remove(filepath.Dir(s.TrashAbs(trashRel)))
	return nil
}

// PurgeTrash permanently deletes a trash slot.
func (s *LocalStore) PurgeTrash(trashRel string) error {
	slot, _, _ := strings.Cut(trashRel, "/")
	if slot == "" || strings.Contains(slot, "..") {
		return ErrInvalidPath
	}
	return os.RemoveAll(filepath.Join(s.root, ".trash", slot))
}

// CleanTemp removes stale in-flight files older than maxAge.
func (s *LocalStore) CleanTemp(maxAge time.Duration) error {
	dir := filepath.Join(s.root, ".tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// Walk visits every file and directory in owner's tree (not the root itself).
func (s *LocalStore) Walk(ctx context.Context, owner OwnerRef, fn func(FileInfo) error) error {
	base := filepath.Join(s.root, owner.dir())
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == base && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == base {
			return nil
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil // skip symlinks, sockets, devices
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		return fn(FileInfo{
			Owner: owner, Path: filepath.ToSlash(rel),
			Size: info.Size(), ModTime: info.ModTime(), IsDir: d.IsDir(),
		})
	})
}

// Owners lists the user and space directories present on disk.
func (s *LocalStore) Owners() ([]OwnerRef, error) {
	var out []OwnerRef
	for _, kind := range []struct{ kind, dir string }{{"user", "users"}, {"space", "shared"}} {
		entries, err := os.ReadDir(filepath.Join(s.root, kind.dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, OwnerRef{Kind: kind.kind, Name: e.Name()})
			}
		}
	}
	return out, nil
}

// HashFile returns the SHA-256 of the file at abs.
func HashFile(abs string) (string, int64, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Best effort: some filesystems (e.g. exFAT via FUSE) reject fsync on directories.
	_ = d.Sync()
	return nil
}
