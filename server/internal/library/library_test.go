package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/events"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

type fixture struct {
	svc   *Service
	db    *metadata.DB
	root  string
	alice *metadata.User
	bob   *metadata.User
}

func newFixture(t *testing.T, scope DedupScope) *fixture {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "library")
	os.MkdirAll(root, 0o750)
	if _, err := volume.Init(root, volume.RoleLibrary); err != nil {
		t.Fatal(err)
	}
	db, err := metadata.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fs := storage.NewLocalStore(root, storage.LinkOff)
	fs.Prepare()
	mode, err := storage.ResolveLinkMode(root, "auto")
	if err != nil {
		t.Fatal(err)
	}
	fs.SetLinkMode(mode)
	guard := volume.NewGuard(root, volume.RoleLibrary, "")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := New(db, fs, events.NewBus(log), guard, Options{DedupScope: scope, TrashRetention: time.Hour}, log)
	f := &fixture{svc: svc, db: db, root: root}
	for _, name := range []string{"alice", "bob"} {
		u := &metadata.User{Username: name, PasswordHash: "x", Role: "member"}
		if err := db.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		if name == "alice" {
			f.alice = u
		} else {
			f.bob = u
		}
	}
	return f
}

func (f *fixture) put(t *testing.T, a Access, p, content string, base *int64) *CommitResult {
	t.Helper()
	tmp, size, sum, err := f.svc.CopyToTemp(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Commit(context.Background(), CommitRequest{
		Access: a, Path: p, TempPath: tmp, SHA256: sum, Size: size, MTime: time.Now(), BaseVersion: base, DeviceName: "laptop",
	})
	if err != nil {
		t.Fatalf("commit %s: %v", p, err)
	}
	return res
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCommitCreatesHumanReadableFiles(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	a := f.svc.UserAccess(f.alice)
	res := f.put(t, a, "Photos/2024/trip.jpg", "jpeg bytes", nil)
	if res.File.Version != 1 || res.Conflict || res.Unchanged {
		t.Fatalf("unexpected result %+v", res)
	}
	if got := readFile(t, filepath.Join(f.root, "users/alice/Photos/2024/trip.jpg")); got != "jpeg bytes" {
		t.Fatalf("disk content %q", got)
	}
	// Parent directories are indexed too.
	items, err := f.svc.List(context.Background(), a, "Photos", "", 100)
	if err != nil || len(items) != 1 || !items[0].IsDir || items[0].Name != "2024" {
		t.Fatalf("listing Photos: %v %+v", err, items)
	}
	if res.File.Mime != "image/jpeg" {
		t.Errorf("mime %q", res.File.Mime)
	}
}

func TestIdenticalReuploadIsNoop(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	a := f.svc.UserAccess(f.alice)
	first := f.put(t, a, "doc.pdf", "pdf", nil)
	again := f.put(t, a, "doc.pdf", "pdf", nil)
	if !again.Unchanged || again.File.ID != first.File.ID {
		t.Fatalf("expected unchanged, got %+v", again)
	}
}

func TestConflictKeepsBoth(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	a := f.svc.UserAccess(f.alice)
	f.put(t, a, "notes.txt", "v1", nil)
	stale := int64(0)
	res := f.put(t, a, "notes.txt", "edited elsewhere", &stale)
	if !res.Conflict || !strings.HasPrefix(res.File.Path, "notes (conflict from laptop ") || !strings.HasSuffix(res.File.Path, ").txt") {
		t.Fatalf("expected conflict copy, got %+v", res.File.Path)
	}
	if got := readFile(t, filepath.Join(f.root, "users/alice/notes.txt")); got != "v1" {
		t.Fatalf("original overwritten: %q", got)
	}
}

func TestReplaceKeepsOldVersionInTrash(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	a := f.svc.UserAccess(f.alice)
	first := f.put(t, a, "budget.xlsx", "v1", nil)
	base := first.File.Version
	res := f.put(t, a, "budget.xlsx", "v2", &base)
	if res.Conflict || res.File.ID != first.File.ID || res.File.Version != 2 {
		t.Fatalf("expected in-place new version, got %+v", res.File)
	}
	if got := readFile(t, filepath.Join(f.root, "users/alice/budget.xlsx")); got != "v2" {
		t.Fatal("new content not in place")
	}
	trash, _ := f.svc.ListTrash(context.Background(), a)
	if len(trash) != 1 || trash[0].TrashReason != "replaced" {
		t.Fatalf("old version not in trash: %+v", trash)
	}
	if got := readFile(t, f.svc.Store().TrashAbs(trash[0].TrashPath)); got != "v1" {
		t.Fatal("trashed version content wrong")
	}
}

func TestDedupAcrossUsers(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	if f.svc.Store().LinkMode() == storage.LinkOff {
		t.Skip("filesystem supports neither reflinks nor hardlinks")
	}
	alice, bob := f.svc.UserAccess(f.alice), f.svc.UserAccess(f.bob)
	f.put(t, alice, "family/beach.jpg", "the same photo", nil)
	res := f.put(t, bob, "Phone/IMG_0001.jpg", "the same photo", nil)
	if res.Linked == "" || res.Linked == storage.LinkOff {
		t.Fatalf("expected a linked copy, got %q", res.Linked)
	}
	pa := filepath.Join(f.root, "users/alice/family/beach.jpg")
	pb := filepath.Join(f.root, "users/bob/Phone/IMG_0001.jpg")
	if res.Linked == storage.LinkHardlink {
		sa, _ := os.Stat(pa)
		sb, _ := os.Stat(pb)
		if sa.Sys().(*syscall.Stat_t).Ino != sb.Sys().(*syscall.Stat_t).Ino {
			t.Fatal("expected shared inode")
		}
	}
	// Bob deleting his copy must not affect Alice's.
	if err := f.svc.Trash(context.Background(), bob, res.File); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, pa); got != "the same photo" {
		t.Fatal("alice's copy damaged")
	}
	// Bob updating his copy (new file + rename) must not change Alice's either.
	res2 := f.put(t, bob, "Phone/IMG_0002.jpg", "the same photo", nil)
	base := res2.File.Version
	f.put(t, bob, "Phone/IMG_0002.jpg", "bob edited it", &base)
	if got := readFile(t, pa); got != "the same photo" {
		t.Fatal("alice's copy changed by bob's edit")
	}
}

func TestLinkExistingAndScope(t *testing.T) {
	ctx := context.Background()
	for _, scope := range []DedupScope{ScopeHousehold, ScopeUser} {
		f := newFixture(t, scope)
		alice := f.svc.UserAccess(f.alice)
		res := f.put(t, alice, "a.bin", "shared content", nil)
		have, err := f.svc.FindForUpload(ctx, f.bob, res.File.SHA256, res.File.Size)
		if err != nil {
			t.Fatal(err)
		}
		if want := scope == ScopeHousehold; have != want {
			t.Fatalf("scope %s: bob sees alice's content = %v", scope, have)
		}
		if scope == ScopeHousehold {
			bob := f.svc.UserAccess(f.bob)
			lr, err := f.svc.LinkExisting(ctx, CommitRequest{Access: bob, Path: "b.bin", SHA256: res.File.SHA256, Size: res.File.Size, MTime: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(f.root, "users/bob/b.bin")); got != "shared content" || lr.File.Owner.ID != f.bob.ID {
				t.Fatal("linked file wrong")
			}
		}
	}
}

func TestMoveDirectoryMovesDescendants(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	ctx := context.Background()
	a := f.svc.UserAccess(f.alice)
	f.put(t, a, "Trips/Goa/1.jpg", "1", nil)
	f.put(t, a, "Trips/Goa/sub/2.jpg", "2", nil)
	dir, _ := f.db.ActiveFileByPath(ctx, nil, a.Owner, "Trips/Goa")
	if _, err := f.svc.Move(ctx, a, dir, "Archive/Goa 2024", ""); err != nil {
		t.Fatal(err)
	}
	moved, err := f.db.ActiveFileByPath(ctx, nil, a.Owner, "Archive/Goa 2024/sub/2.jpg")
	if err != nil || moved.Parent != "Archive/Goa 2024/sub" {
		t.Fatalf("descendant not moved: %v %+v", err, moved)
	}
	if got := readFile(t, filepath.Join(f.root, "users/alice/Archive/Goa 2024/sub/2.jpg")); got != "2" {
		t.Fatal("disk not moved")
	}
	if _, err := f.svc.Move(ctx, a, moved, "Archive/Goa 2024/sub/2.jpg/x", ""); !errors.Is(err, ErrNotDir) && !errors.Is(err, ErrExists) {
		t.Fatalf("moving under a file should fail, got %v", err)
	}
}

func TestTrashAndRestoreDirectory(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	ctx := context.Background()
	a := f.svc.UserAccess(f.alice)
	f.put(t, a, "Bills/2024/power.pdf", "pdf", nil)
	dir, _ := f.db.ActiveFileByPath(ctx, nil, a.Owner, "Bills")
	if err := f.svc.Trash(ctx, a, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ActiveFileByPath(ctx, nil, a.Owner, "Bills/2024/power.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatal("descendant still active after trashing dir")
	}
	// Something new takes the old name in the meantime.
	f.put(t, a, "Bills/new.pdf", "new", nil)
	trash, _ := f.svc.ListTrash(ctx, a)
	if len(trash) != 1 {
		t.Fatalf("expected 1 trash entry, got %d", len(trash))
	}
	restored, err := f.svc.Restore(ctx, a, trash[0], "")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Path != "Bills (restored)" {
		t.Fatalf("restored to %q", restored.Path)
	}
	if _, err := f.db.ActiveFileByPath(ctx, nil, a.Owner, "Bills (restored)/2024/power.pdf"); err != nil {
		t.Fatal("descendant not restored:", err)
	}
	if got := readFile(t, filepath.Join(f.root, "users/alice/Bills (restored)/2024/power.pdf")); got != "pdf" {
		t.Fatal("disk content not restored")
	}
}

func TestSpacePermissions(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	ctx := context.Background()
	sp, _ := f.db.CreateSpace(ctx, "Family Photos", f.alice.ID)
	f.db.SetSpaceMember(ctx, sp.ID, f.alice.ID, "write")
	f.db.SetSpaceMember(ctx, sp.ID, f.bob.ID, "read")
	aw, err := f.svc.Resolve(ctx, f.alice, sp.ID)
	if err != nil || !aw.Write {
		t.Fatal("alice should have write access")
	}
	f.put(t, aw, "trip.jpg", "x", nil)
	if got := readFile(t, filepath.Join(f.root, "shared/Family Photos/trip.jpg")); got != "x" {
		t.Fatal("space file not under shared/")
	}
	br, err := f.svc.Resolve(ctx, f.bob, sp.ID)
	if err != nil || br.Write {
		t.Fatal("bob should be read-only")
	}
	tmp, size, sum, _ := f.svc.CopyToTemp(strings.NewReader("y"))
	if _, err := f.svc.Commit(ctx, CommitRequest{Access: br, Path: "y.jpg", TempPath: tmp, SHA256: sum, Size: size}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only member wrote to space: %v", err)
	}
	f.db.RemoveSpaceMember(ctx, sp.ID, f.bob.ID)
	if _, err := f.svc.Resolve(ctx, f.bob, sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("non-member should not see the space")
	}
}

func TestReadOnlyWhenDiskMissing(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	a := f.svc.UserAccess(f.alice)
	os.Remove(filepath.Join(f.root, volume.MarkerName))
	f.svc.Guard().Refresh()
	if _, _, _, err := f.svc.CopyToTemp(strings.NewReader("x")); !errors.Is(err, volume.ErrUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
	if _, err := f.svc.Mkdir(context.Background(), a, "x", ""); !errors.Is(err, volume.ErrUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}

func TestQuota(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	f.alice.QuotaBytes = 10
	f.db.UpdateUser(context.Background(), f.alice)
	a := f.svc.UserAccess(f.alice)
	f.put(t, a, "a", "12345678", nil)
	tmp, size, sum, _ := f.svc.CopyToTemp(strings.NewReader("12345"))
	if _, err := f.svc.Commit(context.Background(), CommitRequest{Access: a, Path: "b", TempPath: tmp, SHA256: sum, Size: size}); !errors.Is(err, ErrQuota) {
		t.Fatalf("expected quota error, got %v", err)
	}
}

func TestReindex(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	ctx := context.Background()
	a := f.svc.UserAccess(f.alice)
	f.put(t, a, "keep.txt", "keep", nil)
	gone := f.put(t, a, "gone.txt", "gone", nil)
	// Edit the disk behind the server's back.
	os.Remove(filepath.Join(f.root, "users/alice/gone.txt"))
	os.MkdirAll(filepath.Join(f.root, "users/alice/added"), 0o750)
	os.WriteFile(filepath.Join(f.root, "users/alice/added/new.txt"), []byte("new"), 0o640)
	os.WriteFile(filepath.Join(f.root, "users/alice/keep.txt"), []byte("changed!"), 0o640)
	os.MkdirAll(filepath.Join(f.root, "users/carol"), 0o750)

	rep, err := f.svc.Reindex(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 2 || rep.Updated != 1 || rep.Removed != 1 || len(rep.UnknownOwners) != 1 {
		t.Fatalf("unexpected report %+v", rep)
	}
	if g, err := f.db.FileByID(ctx, nil, gone.File.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file still indexed: %v %+v", err, g)
	}
	k, _ := f.db.ActiveFileByPath(ctx, nil, a.Owner, "keep.txt")
	if k.Version != 2 || k.Size != 8 {
		t.Fatalf("changed file not updated: %+v", k)
	}
	// A second run is a no-op.
	rep, _ = f.svc.Reindex(ctx, nil)
	if rep.Added+rep.Updated+rep.Removed != 0 {
		t.Fatalf("reindex not idempotent: %+v", rep)
	}
}

func TestImport(t *testing.T) {
	f := newFixture(t, ScopeHousehold)
	ctx := context.Background()
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "2019"), 0o750)
	os.WriteFile(filepath.Join(src, "2019/a.jpg"), []byte("photo a"), 0o640)
	os.WriteFile(filepath.Join(src, "2019/a-copy.jpg"), []byte("photo a"), 0o640)
	os.WriteFile(filepath.Join(src, ".DS_Store"), []byte("junk"), 0o640)
	old := time.Date(2019, 5, 1, 10, 0, 0, 0, time.UTC)
	os.Chtimes(filepath.Join(src, "2019/a.jpg"), old, old)

	a := f.svc.UserAccess(f.alice)
	rep, err := f.svc.Import(ctx, a, src, "Old Laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 2 || rep.Imported != 2 || len(rep.Errors) != 0 {
		t.Fatalf("unexpected report %+v", rep)
	}
	if f.svc.Store().LinkMode() != storage.LinkOff && rep.Linked != 1 {
		t.Fatalf("duplicate within import should be linked: %+v", rep)
	}
	got, _ := f.db.ActiveFileByPath(ctx, nil, a.Owner, "Old Laptop/2019/a.jpg")
	if got == nil || !got.MTime.Equal(old) {
		t.Fatalf("mtime not preserved: %+v", got)
	}
	// Re-running the import changes nothing.
	rep, _ = f.svc.Import(ctx, a, src, "Old Laptop", nil)
	if rep.Unchanged != 2 || rep.Imported != 0 {
		t.Fatalf("re-import not idempotent: %+v", rep)
	}
}
