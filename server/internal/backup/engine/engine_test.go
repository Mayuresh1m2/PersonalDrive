package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/crypto"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
)

const pass = "test passphrase 123"

func init() { crypto.KDFParams.Memory = 8 * 1024 }

var deepArchive = repo.Classes{Hot: "STANDARD", Cold: "DEEP_ARCHIVE"}

type fixture struct {
	t     *testing.T
	store objectstore.Store
	sim   *objectstore.ArchiveSim
	repo  *repo.Repo
	lib   string
	state string
}

func newFixture(t *testing.T, archive bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	var store objectstore.Store = objectstore.NewLocalUnchecked(filepath.Join(dir, "remote"))
	f := &fixture{t: t, lib: filepath.Join(dir, "library"), state: filepath.Join(dir, "state")}
	if archive {
		f.sim = objectstore.NewArchiveSim(store)
		store = f.sim
	}
	f.store = store
	ctx := context.Background()
	if _, err := repo.Init(ctx, store, repo.InitOptions{Passphrase: pass, PackSize: 1 << 20, Classes: deepArchive}); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(ctx, store, pass, filepath.Join(f.state, "cache"), deepArchive)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = r
	os.MkdirAll(f.lib, 0o750)
	return f
}

func (f *fixture) write(rel string, data []byte) {
	p := filepath.Join(f.lib, rel)
	os.MkdirAll(filepath.Dir(p), 0o750)
	if err := os.WriteFile(p, data, 0o640); err != nil {
		f.t.Fatal(err)
	}
}

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func (f *fixture) backup() *repo.Snapshot {
	f.t.Helper()
	snap, err := Backup(context.Background(), f.repo, BackupOptions{
		Sources:    []Source{{Name: "library", Path: f.lib, Exclude: []string{".tmp"}}},
		StagingDir: filepath.Join(f.state, "staging"), Host: "test",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return snap
}

func (f *fixture) restore(snapshot, target string, opts RestoreOptions) error {
	opts.Snapshot = snapshot
	opts.Targets = map[string]string{"library": target}
	opts.Pricing = DefaultPricing()
	opts.CacheDir = filepath.Join(f.state, "restore-cache")
	plan, err := PlanRestore(context.Background(), f.repo, opts)
	if err != nil {
		return err
	}
	return Restore(context.Background(), f.repo, plan, opts)
}

func sameTree(t *testing.T, a, b string, skip ...string) {
	t.Helper()
	filepath.Walk(a, func(p string, info os.FileInfo, err error) error {
		rel, _ := filepath.Rel(a, p)
		for _, s := range skip {
			if rel == s || strings.HasPrefix(rel, s+"/") {
				return filepath.SkipDir
			}
		}
		if info.IsDir() {
			return nil
		}
		want, _ := os.ReadFile(p)
		got, err := os.ReadFile(filepath.Join(b, rel))
		if err != nil || !bytes.Equal(want, got) {
			t.Errorf("%s differs after restore: %v", rel, err)
		}
		st, _ := os.Stat(filepath.Join(b, rel))
		if st != nil && !st.ModTime().Equal(info.ModTime()) {
			t.Errorf("%s mtime %v, want %v", rel, st.ModTime(), info.ModTime())
		}
		return nil
	})
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	f := newFixture(t, false)
	f.write("users/alice/photo.jpg", randBytes(3<<20, 1))
	f.write("users/alice/notes.txt", []byte(strings.Repeat("compressible text ", 10000)))
	f.write("users/bob/same-photo.jpg", randBytes(3<<20, 1)) // duplicate content
	f.write("users/bob/empty.txt", nil)
	f.write(".tmp/in-flight", []byte("excluded"))
	old := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	os.Chtimes(filepath.Join(f.lib, "users/alice/photo.jpg"), old, old)

	snap := f.backup()
	if snap.Stats.Files != 4 {
		t.Fatalf("files = %d, want 4 (.tmp excluded)", snap.Stats.Files)
	}
	if snap.Stats.BytesDeduped < 3<<20 {
		t.Errorf("duplicate photo not deduped: %+v", snap.Stats)
	}
	if snap.Stats.BytesStored >= snap.Stats.BytesNew {
		t.Errorf("compression had no effect: stored %d new %d", snap.Stats.BytesStored, snap.Stats.BytesNew)
	}

	out := filepath.Join(t.TempDir(), "restored")
	if err := f.restore("latest", out, RestoreOptions{Local: true}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.lib, out, ".tmp")
	if _, err := os.Stat(filepath.Join(out, ".tmp")); err == nil {
		t.Error("excluded dir was restored")
	}

	// Content is encrypted at rest: no plaintext in the repository.
	filepath.Walk(filepath.Dir(f.lib)+"/remote", func(p string, info os.FileInfo, err error) error {
		if info != nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte("compressible text")) || bytes.Contains(b, []byte("alice")) {
				t.Errorf("plaintext visible in %s", p)
			}
		}
		return nil
	})
}

func TestIncrementalBackupReadsOnlyChanges(t *testing.T) {
	f := newFixture(t, false)
	for i := 0; i < 10; i++ {
		f.write(filepath.Join("users/a", string(rune('a'+i))+".bin"), randBytes(500<<10, int64(i)))
	}
	first := f.backup()
	f.write("users/a/new.bin", randBytes(200<<10, 99))
	second := f.backup()
	if second.Stats.FilesUnchanged != 10 || second.Stats.BytesRead != 200<<10 {
		t.Fatalf("incremental read too much: %+v", second.Stats)
	}
	if second.Parent != first.ID {
		t.Errorf("parent = %s, want %s", second.Parent, first.ID)
	}
	snaps, _ := f.repo.Snapshots(context.Background())
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d", len(snaps))
	}
	// Restore the first snapshot: new.bin must not be there.
	out := t.TempDir()
	if err := f.restore(first.ID[:8], out, RestoreOptions{Local: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "users/a/new.bin")); err == nil {
		t.Fatal("file from a later snapshot appeared in an older restore")
	}
}

func TestDeepArchiveRestoreNeedsThaw(t *testing.T) {
	f := newFixture(t, true)
	f.write("users/alice/video.mp4", randBytes(4<<20, 7))
	f.write("users/alice/doc.pdf", randBytes(100<<10, 8))
	f.backup()

	out := t.TempDir()
	opts := RestoreOptions{Snapshot: "latest", Targets: map[string]string{"library": out}, Pricing: DefaultPricing(), NoWait: true, Tier: "Bulk"}
	plan, err := PlanRestore(context.Background(), f.repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ArchivedPacks == 0 || plan.Estimate.Total <= 0 || plan.Estimate.ETA != 48*time.Hour {
		t.Fatalf("plan should need thawing and cost money: %s", plan.Summary())
	}
	if err := Restore(context.Background(), f.repo, plan, opts); !errors.Is(err, ErrThawPending) {
		t.Fatalf("expected ErrThawPending, got %v", err)
	}
	if f.sim.Requests.Load() == 0 {
		t.Fatal("no restore requests issued")
	}
	f.sim.CompleteRestores()
	plan, _ = PlanRestore(context.Background(), f.repo, opts)
	if plan.ArchivedPacks != 0 {
		t.Fatalf("packs still archived after thaw: %d", plan.ArchivedPacks)
	}
	if err := Restore(context.Background(), f.repo, plan, opts); err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.lib, out)
}

func TestPartialRestore(t *testing.T) {
	f := newFixture(t, false)
	f.write("users/alice/a.bin", randBytes(2<<20, 1))
	f.write("users/bob/b.bin", randBytes(2<<20, 2))
	f.backup()
	out := t.TempDir()
	if err := f.restore("latest", out, RestoreOptions{Local: true, Include: []string{"library/users/bob"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "users/bob/b.bin")); err != nil {
		t.Fatal("bob's file not restored")
	}
	if _, err := os.Stat(filepath.Join(out, "users/alice")); err == nil {
		t.Fatal("alice's files restored despite include filter")
	}
}

func TestRestoreIsResumable(t *testing.T) {
	f := newFixture(t, false)
	f.write("x/1.bin", randBytes(1<<20, 1))
	f.write("x/2.bin", randBytes(1<<20, 2))
	f.backup()
	out := t.TempDir()
	if err := f.restore("latest", out, RestoreOptions{Local: true, Include: []string{"library/x/1.bin"}}); err != nil {
		t.Fatal(err)
	}
	plan, _ := PlanRestore(context.Background(), f.repo, RestoreOptions{Snapshot: "latest", Targets: map[string]string{"library": out}, Local: true})
	if plan.AlreadyPresent != 1 || len(plan.files) != 1 {
		t.Fatalf("resume should skip restored file: present=%d todo=%d", plan.AlreadyPresent, len(plan.files))
	}
}

func TestPruneRetentionAndMinimumDuration(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	f.write("a.bin", randBytes(2<<20, 1))
	f.backup()
	os.Remove(filepath.Join(f.lib, "a.bin"))
	f.write("b.bin", randBytes(2<<20, 2))
	f.backup()

	rep, err := Prune(ctx, f.repo, PruneOptions{Retention: Retention{Last: 1}, Pricing: DefaultPricing()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SnapshotsRemoved) != 1 {
		t.Fatalf("expected 1 snapshot removed: %+v", rep)
	}
	// a.bin's cold pack is unused but younger than Deep Archive's 180 days.
	if rep.PacksDeferred == 0 || rep.PacksDeleted != 0 && rep.BytesFreed > 1<<20 {
		t.Fatalf("cold pack should be deferred, not deleted: %+v", rep)
	}
	// Remaining snapshot still restores.
	out := t.TempDir()
	f.sim.CompleteRestores()
	opts := RestoreOptions{Snapshot: "latest", Targets: map[string]string{"library": out}, Pricing: DefaultPricing(), NoWait: true}
	plan, _ := PlanRestore(ctx, f.repo, opts)
	Restore(ctx, f.repo, plan, opts)
	f.sim.CompleteRestores()
	plan, _ = PlanRestore(ctx, f.repo, opts)
	if err := Restore(ctx, f.repo, plan, opts); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "b.bin")); !bytes.Equal(b, randBytes(2<<20, 2)) {
		t.Fatal("restore after prune broken")
	}
	check, err := Check(ctx, f.repo, CheckOptions{})
	if err != nil || !check.OK() {
		t.Fatalf("check after prune: %v %+v", err, check.Errors)
	}
}

func TestPruneDeletesUnusedLocalData(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	f.write("a.bin", randBytes(3<<20, 1))
	f.backup()
	os.Remove(filepath.Join(f.lib, "a.bin"))
	f.write("b.bin", []byte("small"))
	f.backup()
	rep, err := Prune(ctx, f.repo, PruneOptions{Retention: Retention{Last: 1}, Pricing: DefaultPricing()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PacksDeleted == 0 || rep.BytesFreed < 2<<20 {
		t.Fatalf("local data should be freed immediately: %+v", rep)
	}
	if c, _ := Check(ctx, f.repo, CheckOptions{ReadFraction: 1}); !c.OK() {
		t.Fatalf("check failed: %v", c.Errors)
	}
}

func TestCheckFindsProblems(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	f.write("photo.jpg", randBytes(2<<20, 1))
	f.backup()

	c, err := Check(ctx, f.repo, CheckOptions{ReadFraction: 1, LocalSource: "library", LocalPath: f.lib})
	if err != nil || !c.OK() || c.BlobsVerified == 0 || c.FilesCompared != 1 {
		t.Fatalf("clean repo should pass: %v %+v", err, c)
	}
	// Bit rot on the primary disk: same size and mtime, different bytes.
	p := filepath.Join(f.lib, "photo.jpg")
	st, _ := os.Stat(p)
	data, _ := os.ReadFile(p)
	data[100] ^= 0xff
	os.WriteFile(p, data, 0o640)
	os.Chtimes(p, st.ModTime(), st.ModTime())
	c, _ = Check(ctx, f.repo, CheckOptions{LocalSource: "library", LocalPath: f.lib})
	if len(c.LocalMismatch) != 1 {
		t.Fatalf("bit rot not detected: %+v", c)
	}
	// A corrupted pack in the repository.
	var packFile string
	filepath.Walk(filepath.Dir(f.lib)+"/remote/repo/data", func(p string, info os.FileInfo, err error) error {
		if info != nil && !info.IsDir() && info.Size() > 1<<20 {
			packFile = p
		}
		return nil
	})
	b, _ := os.ReadFile(packFile)
	b[10] ^= 0xff
	os.WriteFile(packFile, b, 0o640)
	c, _ = Check(ctx, f.repo, CheckOptions{ReadFraction: 1})
	if c.OK() {
		t.Fatal("corrupted pack not detected")
	}
}

// failingStore fails every Put after a budget, simulating a crash or network loss.
type failingStore struct {
	objectstore.Store
	budget atomic.Int64
}

func (s *failingStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, o objectstore.PutOptions) error {
	if strings.HasPrefix(key, "data/") && s.budget.Add(-1) < 0 {
		return context.Canceled // non-retryable, like the process being killed
	}
	return s.Store.Put(ctx, key, r, size, o)
}

func TestInterruptedBackupResumesWithoutReupload(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		f.write(filepath.Join("big", string(rune('a'+i))+".bin"), randBytes(1<<20, int64(i)))
	}
	fs := &failingStore{Store: f.store}
	fs.budget.Store(6)
	r, _ := repo.Open(ctx, fs, pass, filepath.Join(f.state, "cache"), deepArchive)
	_, err := Backup(ctx, r, BackupOptions{
		Sources: []Source{{Name: "library", Path: f.lib}}, StagingDir: filepath.Join(f.state, "staging"),
		Host: "test", CheckpointPacks: 2, Uploaders: 1,
	})
	if err == nil {
		t.Fatal("expected the interrupted backup to fail")
	}
	r.RemoveLocks(ctx)
	// Second run: packs indexed at checkpoints are not uploaded again.
	r2, _ := repo.Open(ctx, f.store, pass, filepath.Join(f.state, "cache"), deepArchive)
	snap, err := Backup(ctx, r2, BackupOptions{Sources: []Source{{Name: "library", Path: f.lib}}, StagingDir: filepath.Join(f.state, "staging"), Host: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Stats.BytesDeduped < 4<<20 {
		t.Fatalf("resumed backup re-uploaded checkpointed data: %+v", snap.Stats)
	}
	out := t.TempDir()
	if err := f.restore("latest", out, RestoreOptions{Local: true}); err != nil {
		t.Fatal(err)
	}
	sameTree(t, f.lib, out)
}

func TestWrongPassphraseAndRecoveryKey(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalUnchecked(t.TempDir())
	rk, err := repo.Init(ctx, store, repo.InitOptions{Passphrase: pass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, store, "wrong passphrase!", t.TempDir(), repo.Classes{}); !errors.Is(err, crypto.ErrWrongSecret) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	r, err := repo.Open(ctx, store, strings.ToLower(rk), t.TempDir(), repo.Classes{})
	if err != nil {
		t.Fatal("recovery key should open the repo:", err)
	}
	if err := r.ChangePassphrase(ctx, "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, store, pass, t.TempDir(), repo.Classes{}); err == nil {
		t.Fatal("old passphrase still works")
	}
	if _, err := repo.Open(ctx, store, "a brand new passphrase", t.TempDir(), repo.Classes{}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Init(ctx, store, repo.InitOptions{Passphrase: pass}); !errors.Is(err, repo.ErrRepoExists) {
		t.Fatal("init over an existing repo must fail")
	}
}

func TestApplyRetention(t *testing.T) {
	var snaps []*repo.Snapshot
	base := time.Date(2026, 1, 1, 2, 0, 0, 0, time.Local)
	for d := 0; d < 60; d++ {
		snaps = append(snaps, &repo.Snapshot{ID: string(rune(d + 100)), Time: base.AddDate(0, 0, d)})
	}
	keep, remove := ApplyRetention(snaps, Retention{Daily: 7, Monthly: 3})
	if len(keep)+len(remove) != 60 {
		t.Fatal("lost snapshots")
	}
	// 7 most recent days + the latest in each of the 3 most recent months
	// (two of which overlap with the daily set).
	if len(keep) < 8 || len(keep) > 10 {
		t.Fatalf("kept %d snapshots", len(keep))
	}
}

func TestStatsSanity(t *testing.T) {
	f := newFixture(t, true)
	f.write("users/a/x.bin", randBytes(5<<20, 3))
	f.write("users/b/x-copy.bin", randBytes(5<<20, 3))
	s := f.backup()
	t.Logf("stats: files=%d scanned=%d read=%d new=%d dedup=%d stored=%d uploaded=%d packs=%d",
		s.Stats.Files, s.Stats.BytesScanned, s.Stats.BytesRead, s.Stats.BytesNew, s.Stats.BytesDeduped,
		s.Stats.BytesStored, s.Stats.BytesUploaded, s.Stats.PacksUploaded)
	t.Logf("puts=%d", f.sim.Puts.Load())
	if s.Stats.BytesNew != 5<<20 || s.Stats.BytesDeduped != 5<<20 {
		t.Fatal("dedup accounting wrong")
	}
}
