package volume

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMarkerAndGuard(t *testing.T) {
	dir := t.TempDir()
	g := NewGuard(dir, RoleLibrary, "")
	if err := g.Writable(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected unavailable without marker, got %v", err)
	}
	m, err := Init(dir, RoleLibrary)
	if err != nil {
		t.Fatal(err)
	}
	if m2, err := Init(dir, RoleLibrary); err != nil || m2.ID != m.ID {
		t.Fatalf("re-init should be idempotent: %v %v", m2, err)
	}
	if _, err := Init(dir, RoleBackup); err == nil {
		t.Fatal("changing role must fail")
	}
	if s := g.Refresh(); !s.OK || s.TotalBytes == 0 {
		t.Fatalf("expected OK with usage, got %+v", s)
	}
	g.SetExpectedID("some-other-disk")
	if s := g.Refresh(); s.OK {
		t.Fatal("a different disk must be refused")
	}
	g.SetExpectedID(m.ID)
	g.Refresh()
	// Simulate unplugging the disk.
	os.Remove(filepath.Join(dir, MarkerName))
	if s := g.Refresh(); s.OK {
		t.Fatal("missing marker must make the volume unavailable")
	}
}

func TestInitRequiresExistingDir(t *testing.T) {
	if _, err := Init(filepath.Join(t.TempDir(), "not-mounted"), RoleLibrary); err == nil {
		t.Fatal("expected error for missing mount point")
	}
}
