// Package drill runs the Phase 2 disaster drill end to end: build a library
// with users, back it up to an S3-compatible server, lose the machine, and
// rebuild it from the recovery kit alone.
package drill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/app"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/crypto"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/engine"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
	backupsvc "github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/service"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/library"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/tlsutil"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

func init() { crypto.KDFParams.Memory = 8 * 1024 }

const passphrase = "drill passphrase 2026"

type machine struct {
	dir, state, lib, configPath string
	cfg                         *config.Config
	app                         *app.App
}

func newMachine(t *testing.T, name string, target config.BackupTarget) *machine {
	t.Helper()
	m := &machine{dir: filepath.Join(t.TempDir(), name)}
	m.state, m.lib, m.configPath = filepath.Join(m.dir, "ssd/state"), filepath.Join(m.dir, "disk-a/library"), filepath.Join(m.dir, "config.yaml")
	os.MkdirAll(m.lib, 0o750)
	return m
}

func (m *machine) open(t *testing.T) {
	t.Helper()
	cfg, err := config.Load(m.configPath)
	if err != nil {
		t.Fatal(err)
	}
	m.cfg = cfg
	m.app, err = app.Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	if err != nil {
		t.Fatal(err)
	}
}

func (m *machine) close() {
	if m.app != nil {
		m.app.Close()
		m.app = nil
	}
}

// inventory is path → sha256 of every active file, read from disk.
func (m *machine) inventory(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(m.lib, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(m.lib, p)
		if strings.HasPrefix(rel, ".tmp") {
			return nil
		}
		f, _ := os.Open(p)
		h := sha256.New()
		io.Copy(h, f)
		f.Close()
		out[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out
}

func put(t *testing.T, lib *library.Service, a library.Access, path, content string) {
	t.Helper()
	tmp, size, sum, err := lib.CopyToTemp(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Commit(context.Background(), library.CommitRequest{Access: a, Path: path, TempPath: tmp, SHA256: sum, Size: size, MTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func same(t *testing.T, label string, want, got map[string]string) {
	t.Helper()
	var missing, wrong []string
	for p, h := range want {
		switch g, ok := got[p]; {
		case !ok:
			missing = append(missing, p)
		case g != h:
			wrong = append(wrong, p)
		}
	}
	sort.Strings(missing)
	if len(missing)+len(wrong) > 0 || len(got) != len(want) {
		t.Fatalf("%s: %d files expected, %d restored; missing %v; wrong content %v", label, len(want), len(got), missing, wrong)
	}
}

func TestDisasterDrill(t *testing.T) {
	ctx := context.Background()
	s3srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	defer s3srv.Close()
	target := config.BackupTarget{
		Name: "offsite", Type: "s3", Bucket: "pdrive-drill", Prefix: "home", Region: "us-east-1",
		Endpoint: s3srv.URL, AccessKeyID: "test", SecretAccessKey: "test", PathStyle: true,
		StorageClass: "STANDARD", // the fake has no archive tiers; archive thawing is covered by engine tests
		Retention:    config.Retention{Last: 10},
	}

	// ---- Machine 1: the original server ------------------------------------------
	old := newMachine(t, "old", target)
	volume.Init(old.lib, volume.RoleLibrary)
	cfg := config.Default()
	cfg.Paths.State, cfg.Paths.Library = old.state, old.lib
	cfg.Server.TLS, cfg.Server.MDNS = true, false
	cfg.Backup.Targets = []config.BackupTarget{target}
	if err := cfg.Save(old.configPath); err != nil {
		t.Fatal(err)
	}
	old.open(t)
	a := old.app
	alice, err := a.Auth.CreateUser(ctx, "alice", "Alice", "alice long password", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := a.Auth.CreateUser(ctx, "bob", "Bob", "bob's long password", "member")
	sp, _ := a.DB.CreateSpace(ctx, "Family Photos", alice.ID)
	a.DB.SetSpaceMember(ctx, sp.ID, bob.ID, "read")
	tok, err := a.Auth.Login(ctx, "bob", "bob's long password", "Bob's phone", "android")
	if err != nil {
		t.Fatal(err)
	}
	tls, err := tlsutil.Load(old.cfg.StatePath("tls"))
	if err != nil {
		t.Fatal(err)
	}
	caFingerprint := tls.CAFingerprint()

	aliceAcc, bobAcc := a.Lib.UserAccess(alice), a.Lib.UserAccess(bob)
	spaceAcc, _ := a.Lib.SpaceAccess(ctx, sp.ID)
	put(t, a.Lib, aliceAcc, "Photos/2024/goa.jpg", strings.Repeat("goa beach ", 50000))
	put(t, a.Lib, aliceAcc, "Documents/Taxes/2025.pdf", "tax return")
	put(t, a.Lib, bobAcc, "Phone/IMG_0001.jpg", strings.Repeat("goa beach ", 50000)) // dup of alice's
	put(t, a.Lib, spaceAcc, "Trips/manali.jpg", strings.Repeat("snow ", 80000))
	put(t, a.Lib, bobAcc, "notes.txt", "to be deleted")

	if err := backupsvc.SavePassphrase(old.cfg.PassphrasePath(), passphrase); err != nil {
		t.Fatal(err)
	}
	store, err := backupsvc.OpenStore(ctx, &old.cfg.Backup.Targets[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.(*objectstore.S3).EnsureBucket(ctx, true); err != nil {
		t.Fatal(err)
	}
	recoveryKey, err := repo.Init(ctx, store, repo.InitOptions{Passphrase: passphrase, PackSize: 256 << 10, Classes: backupsvc.Classes(&target)})
	if err != nil {
		t.Fatal(err)
	}
	snap1, err := a.Backup.Backup(ctx, "offsite", nil)
	if err != nil {
		t.Fatal(err)
	}
	want1 := old.inventory(t)

	// Changes between backups: new file, deletion (to trash), rename.
	put(t, a.Lib, aliceAcc, "Photos/2024/new.jpg", "new photo")
	notes, _ := a.DB.ActiveFileByPath(ctx, nil, bobAcc.Owner, "notes.txt")
	a.Lib.Trash(ctx, bobAcc, notes)
	tax, _ := a.DB.ActiveFileByPath(ctx, nil, aliceAcc.Owner, "Documents/Taxes/2025.pdf")
	a.Lib.Move(ctx, aliceAcc, tax, "Documents/Taxes/FY2025.pdf", "")
	snap2, err := a.Backup.Backup(ctx, "offsite", nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap2.Stats.FilesUnchanged == 0 {
		t.Errorf("second backup should reuse unchanged files: %+v", snap2.Stats)
	}
	want2 := old.inventory(t)
	if rep, err := a.Backup.Check(ctx, "offsite", a.Backup.ScheduledCheckOptions(&old.cfg.Backup.Targets[0])); err != nil || !rep.OK() {
		t.Fatalf("check before disaster: %v %+v", err, rep)
	}
	if _, err := a.Backup.Prune(ctx, "offsite", false); err != nil {
		t.Fatal(err)
	}
	old.close()

	// ---- Disaster: the laptop and both disks are gone -----------------------------
	os.RemoveAll(old.dir)
	kitPath := filepath.Join(t.TempDir(), "kit.yaml")
	kf, _ := os.Create(kitPath)
	backupsvc.WriteKit(kf, backupsvc.Kit{Server: "old", Created: time.Now(), Target: target, RecoveryKey: recoveryKey})
	kf.Close()

	// ---- Machine 2: rebuilt from the recovery kit ----------------------------------
	nm := newMachine(t, "new", target)
	kit, err := backupsvc.ReadKit(kitPath)
	if err != nil {
		t.Fatal(err)
	}
	store2, _ := backupsvc.OpenStore(ctx, &kit.Target)
	r, err := repo.Open(ctx, store2, strings.ToLower(kit.RecoveryKey), filepath.Join(nm.state, "backup/cache"), backupsvc.Classes(&kit.Target))
	if err != nil {
		t.Fatal(err)
	}
	if err := backupsvc.RestoreMeta(ctx, r, backupsvc.RestoreMetaOptions{
		Snapshot: "latest", StateDir: nm.state, ConfigPath: nm.configPath, LibraryPath: nm.lib, Target: kit.Target,
	}); err != nil {
		t.Fatal(err)
	}
	// Server opened mid-restore is read-only.
	nm.open(t)
	if err := nm.app.Guard.Writable(); err == nil {
		t.Fatal("server must be read-only while the library restore is pending")
	}
	nm.close()

	opts := engine.RestoreOptions{Snapshot: "latest", Targets: map[string]string{backupsvc.SourceLibrary: nm.lib}, Pricing: engine.DefaultPricing()}
	plan, err := engine.PlanRestore(ctx, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := backupsvc.RestoreLibrary(ctx, r, plan, opts, nm.state); err != nil {
		t.Fatal(err)
	}
	same(t, "latest snapshot", want2, nm.inventory(t))

	nm.open(t)
	defer nm.close()
	b := nm.app
	if err := b.Guard.Writable(); err != nil {
		t.Fatalf("restored server should be writable: %v", err)
	}
	users, _ := b.DB.ListUsers(ctx)
	if len(users) != 2 {
		t.Fatalf("users not restored: %d", len(users))
	}
	if _, err := b.Auth.Login(ctx, "alice", "alice long password", "new laptop", "linux"); err != nil {
		t.Fatal("old password should work:", err)
	}
	if _, err := b.Auth.Refresh(ctx, tok.RefreshToken); err != nil {
		t.Fatal("bob's phone should still be signed in:", err)
	}
	tls2, _ := tlsutil.Load(nm.cfg.StatePath("tls"))
	if tls2.CAFingerprint() != caFingerprint {
		t.Fatal("TLS CA changed: paired devices would stop trusting the server")
	}
	if sps, _ := b.DB.ListSpaces(ctx, bob.ID); len(sps) != 1 || sps[0].Permission != "read" {
		t.Fatalf("space membership not restored: %+v", sps)
	}
	trash, _ := b.Lib.ListTrash(ctx, b.Lib.UserAccess(users[1]))
	if len(trash) != 1 {
		t.Fatalf("bob's trash not restored: %d entries", len(trash))
	}
	if _, err := b.Lib.Restore(ctx, b.Lib.UserAccess(users[1]), trash[0], ""); err != nil {
		t.Fatal("restoring from trash after disaster recovery:", err)
	}
	// The restored server keeps working: new uploads and the next backup.
	put(t, b.Lib, b.Lib.UserAccess(users[0]), "after-recovery.txt", "life goes on")
	backupsvc.SavePassphrase(nm.cfg.PassphrasePath(), passphrase)
	if _, err := b.Backup.Backup(ctx, "offsite", nil); err != nil {
		t.Fatal("first backup after recovery:", err)
	}
	if n, _ := b.Runner.Backfill(ctx); n == 0 {
		t.Error("thumbnails should be queued for regeneration after a restore")
	}

	// ---- An older snapshot restores too ------------------------------------------
	older := t.TempDir()
	opts1 := engine.RestoreOptions{Snapshot: snap1.ID, Targets: map[string]string{backupsvc.SourceLibrary: older}, Pricing: engine.DefaultPricing()}
	plan1, err := engine.PlanRestore(ctx, r, opts1)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Restore(ctx, r, plan1, opts1); err != nil {
		t.Fatal(err)
	}
	same(t, "first snapshot", want1, (&machine{lib: older}).inventory(t))
}
