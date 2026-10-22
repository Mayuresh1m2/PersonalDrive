package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCleanPath(t *testing.T) {
	ok := map[string]string{
		"a/b.jpg":         "a/b.jpg",
		"/a//b/":          "",
		"Photos\\2024\\x": "Photos/2024/x",
		"":                "",
		"ünïcode/日本.png":  "ünïcode/日本.png",
	}
	for in, want := range ok {
		got, err := CleanPath(in)
		if in == "/a//b/" {
			if err == nil {
				t.Errorf("CleanPath(%q) should reject empty segment", in)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../etc/passwd", "a/../../b", "a/./b", ".pdrive-volume", "x/.pdrive-y", "a\x00b", "a\nb", strings.Repeat("x", 256)} {
		if _, err := CleanPath(bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanPath(%q) should fail, got %v", bad, err)
		}
	}
	if _, err := CleanName("a/b"); err == nil {
		t.Error("CleanName should reject slashes")
	}
	if _, err := CleanName(".hidden"); err == nil {
		t.Error("CleanName should reject leading dot")
	}
}

func newStore(t *testing.T) *LocalStore {
	t.Helper()
	s := NewLocalStore(t.TempDir(), LinkOff)
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutNeverOverwrites(t *testing.T) {
	s := newStore(t)
	o := OwnerRef{Kind: "user", Name: "alice"}
	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	fi, err := s.Put(context.Background(), o, "a/b.txt", strings.NewReader("hello"), mt)
	if err != nil {
		t.Fatal(err)
	}
	if fi.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || fi.Size != 5 {
		t.Fatalf("unexpected info %+v", fi)
	}
	st, _ := os.Stat(s.Abs(o, "a/b.txt"))
	if !st.ModTime().Equal(mt) {
		t.Errorf("mtime not preserved: %v", st.ModTime())
	}
	if _, err := s.Put(context.Background(), o, "a/b.txt", strings.NewReader("x"), mt); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	if b, _ := os.ReadFile(s.Abs(o, "a/b.txt")); string(b) != "hello" {
		t.Fatal("original content changed")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), ".tmp"))
	if len(entries) != 0 {
		t.Errorf("temp files left behind: %d", len(entries))
	}
}

func TestTrashRoundTrip(t *testing.T) {
	s := newStore(t)
	o := OwnerRef{Kind: "space", Name: "Family"}
	if _, err := s.Put(context.Background(), o, "x/y.txt", strings.NewReader("y"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	rel, err := s.MoveToTrash(o, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Abs(o, "x")); !os.IsNotExist(err) {
		t.Fatal("dir still present after trash")
	}
	if err := s.RestoreFromTrash(rel, o, "x2"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(s.Abs(o, "x2/y.txt")); string(b) != "y" {
		t.Fatal("restored content wrong")
	}
}

func TestLinkModes(t *testing.T) {
	s := newStore(t)
	mode, err := ResolveLinkMode(s.Root(), "auto")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("filesystem supports %s", mode)
	s.SetLinkMode(mode)
	o := OwnerRef{Kind: "user", Name: "a"}
	if _, err := s.Put(context.Background(), o, "orig.bin", strings.NewReader("same bytes"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	tmp, used, err := s.LinkInto(s.Abs(o, "orig.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Place(tmp, OwnerRef{Kind: "user", Name: "b"}, "copy.bin", time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if used == LinkHardlink {
		a, _ := os.Stat(s.Abs(o, "orig.bin"))
		b, _ := os.Stat(s.Abs(OwnerRef{Kind: "user", Name: "b"}, "copy.bin"))
		if a.Sys().(*syscall.Stat_t).Ino != b.Sys().(*syscall.Stat_t).Ino {
			t.Error("hardlink should share the inode")
		}
	}
	if b, _ := os.ReadFile(s.Abs(OwnerRef{Kind: "user", Name: "b"}, "copy.bin")); string(b) != "same bytes" {
		t.Fatal("linked content wrong")
	}
	if _, err := ResolveLinkMode(s.Root(), "bogus"); err == nil {
		t.Error("expected error for unknown mode")
	}
}
