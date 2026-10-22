package syncer

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/desktop_app/internal/client"
)

// startServer runs a real pdrive server (plain HTTP) with user "alice".
func startServer(t *testing.T) string {
	t.Helper()
	bin, _ := filepath.Abs("../../../server/bin/pdrive")
	if _, err := os.Stat(bin); err != nil {
		if out, err := exec.Command("make", "-C", "../../../server", "build").CombinedOutput(); err != nil {
			t.Fatalf("building server: %v\n%s", err, out)
		}
	}
	dir := t.TempDir()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cfg := filepath.Join(dir, "config.yaml")
	run := func(stdin string, args ...string) {
		cmd := exec.Command(bin, append([]string{"--config", cfg}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pdrive %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(dir, "disk-a"), 0o750)
	run("", "init", "--library", filepath.Join(dir, "disk-a"), "--state", filepath.Join(dir, "state"), "--listen", addr, "--no-tls")
	data, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, []byte(strings.Replace(string(data), "mdns: true", "mdns: false", 1)), 0o600)
	run("alice test password\n", "user", "add", "alice", "--admin")
	srv := exec.Command(bin, "--config", cfg, "serve")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Process.Kill(); srv.Wait() })
	url := "http://" + addr
	for i := 0; i < 100; i++ {
		if resp, err := http.Get(url + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return url
}

type device struct {
	t    *testing.T
	name string
	s    *Syncer
}

func newDevice(t *testing.T, url, name string, pair Pair) *device {
	c, _ := client.New(url, nil, client.Tokens{})
	if err := c.Login(context.Background(), "alice", "alice test password", name); err != nil {
		t.Fatal(err)
	}
	if pair.Local == "" {
		pair.Local = filepath.Join(t.TempDir(), name)
	}
	return &device{t: t, name: name, s: &Syncer{
		C: c, Pair: pair, StateDir: filepath.Join(t.TempDir(), name+"-state"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Settle: time.Millisecond,
	}}
}

func (d *device) sync() *Report {
	d.t.Helper()
	time.Sleep(5 * time.Millisecond) // let mtimes settle
	r, err := d.s.SyncOnce(context.Background())
	if err != nil {
		d.t.Fatalf("%s sync: %v", d.name, err)
	}
	if len(r.Errors) > 0 {
		d.t.Fatalf("%s sync errors: %v", d.name, r.Errors)
	}
	return r
}

func (d *device) write(rel, content string) {
	p := filepath.Join(d.s.Pair.Local, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

func (d *device) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(d.s.Pair.Local, filepath.FromSlash(rel)))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// tree is rel → content for every file.
func (d *device) tree() map[string]string {
	out := map[string]string{}
	filepath.WalkDir(d.s.Pair.Local, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			rel, _ := filepath.Rel(d.s.Pair.Local, p)
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

// converge syncs both devices until nothing changes, then checks they match.
func converge(t *testing.T, a, b *device) map[string]string {
	t.Helper()
	for i := 0; i < 6; i++ {
		ra, rb := a.sync(), b.sync()
		if !ra.Changed() && !rb.Changed() {
			ta, tb := a.tree(), b.tree()
			if fmt.Sprint(ta) != fmt.Sprint(tb) {
				t.Fatalf("devices diverged:\n%s: %v\n%s: %v", a.name, ta, b.name, tb)
			}
			return ta
		}
	}
	t.Fatal("sync did not converge")
	return nil
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestTwoWaySync(t *testing.T) {
	url := startServer(t)
	laptop := newDevice(t, url, "laptop", Pair{Remote: "Sync"})
	desktop := newDevice(t, url, "desktop", Pair{Remote: "Sync"})

	// Create on one side, appears on the other (folders too).
	laptop.write("Docs/plan.txt", "v1")
	laptop.write("photo.jpg", strings.Repeat("x", 3<<20))
	os.MkdirAll(filepath.Join(laptop.s.Pair.Local, "Empty"), 0o755)
	converge(t, laptop, desktop)
	if desktop.read("Docs/plan.txt") != "v1" {
		t.Fatal("file not propagated")
	}
	if st, err := os.Stat(filepath.Join(desktop.s.Pair.Local, "Empty")); err != nil || !st.IsDir() {
		t.Fatal("empty folder not propagated")
	}

	// Edit propagates back.
	desktop.write("Docs/plan.txt", "v2 from desktop")
	converge(t, laptop, desktop)
	if laptop.read("Docs/plan.txt") != "v2 from desktop" {
		t.Fatalf("edit not propagated: %q", laptop.read("Docs/plan.txt"))
	}

	// Conflict: both edit before syncing. Both versions survive everywhere.
	laptop.write("Docs/plan.txt", "v3 laptop")
	desktop.write("Docs/plan.txt", "v3 desktop!")
	laptop.sync()
	tree := converge(t, laptop, desktop)
	var conflictCopy string
	for _, k := range keys(tree) {
		if strings.HasPrefix(k, "Docs/plan (conflict from desktop") {
			conflictCopy = k
		}
	}
	if tree["Docs/plan.txt"] != "v3 laptop" || conflictCopy == "" || tree[conflictCopy] != "v3 desktop!" {
		t.Fatalf("conflict lost data: %v", tree)
	}
	if n := len(keys(tree)); n != 3 {
		t.Fatalf("expected exactly one conflict copy, files: %v", keys(tree))
	}

	// Delete on one side: removed on the other, recoverable from the sync trash.
	laptop.s.Log.Info("delete")
	os.Remove(filepath.Join(laptop.s.Pair.Local, "photo.jpg"))
	converge(t, laptop, desktop)
	if desktop.read("photo.jpg") != "<missing>" {
		t.Fatal("deletion not propagated")
	}
	var trashed bool
	filepath.WalkDir(desktop.s.TrashDir(), func(p string, e fs.DirEntry, err error) error {
		trashed = trashed || (err == nil && e.Name() == "photo.jpg")
		return nil
	})
	if !trashed {
		t.Fatal("locally deleted file should be recoverable from the sync trash")
	}

	// Delete vs edit: the edit wins.
	desktop.write("Docs/keep.txt", "important")
	converge(t, laptop, desktop)
	os.Remove(filepath.Join(laptop.s.Pair.Local, "Docs/keep.txt"))
	desktop.write("Docs/keep.txt", "important, edited")
	desktop.sync()
	tree = converge(t, laptop, desktop)
	if tree["Docs/keep.txt"] != "important, edited" {
		t.Fatalf("edit lost to a concurrent delete: %v", tree)
	}

	// Rename a folder.
	if err := os.Rename(filepath.Join(laptop.s.Pair.Local, "Docs"), filepath.Join(laptop.s.Pair.Local, "Documents")); err != nil {
		t.Fatal(err)
	}
	tree = converge(t, laptop, desktop)
	if tree["Documents/plan.txt"] != "v3 laptop" || desktop.read("Docs/plan.txt") != "<missing>" {
		t.Fatalf("folder rename not mirrored: %v", keys(tree))
	}
}

func TestFirstSyncWithExistingFiles(t *testing.T) {
	url := startServer(t)
	a := newDevice(t, url, "a", Pair{})
	a.write("shared.txt", "from a")
	a.write("same.txt", "identical")
	a.sync()

	// b already has files before its first sync: nothing may be overwritten.
	b := newDevice(t, url, "b", Pair{})
	b.write("shared.txt", "from b, different")
	b.write("same.txt", "identical")
	b.write("only-b.txt", "b")
	tree := converge(t, a, b)
	var copies []string
	for _, k := range keys(tree) {
		if strings.HasPrefix(k, "shared (conflict") {
			copies = append(copies, k)
		}
	}
	if tree["shared.txt"] != "from a" || len(copies) != 1 || tree[copies[0]] != "from b, different" {
		t.Fatalf("first sync lost data: %v", tree)
	}
	if tree["same.txt"] != "identical" || tree["only-b.txt"] != "b" {
		t.Fatalf("unexpected tree: %v", tree)
	}
}

func TestSettleAndIgnore(t *testing.T) {
	url := startServer(t)
	a := newDevice(t, url, "a", Pair{})
	a.s.Settle = time.Hour
	a.write("busy.mov", "still copying")
	a.write(".DS_Store", "junk")
	a.write("~$report.docx", "office lock file")
	r := a.sync()
	if r.Uploaded != 0 || r.Skipped != 1 {
		t.Fatalf("busy file should wait, junk ignored: %+v", r)
	}
	a.s.Settle = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	if r := a.sync(); r.Uploaded != 1 {
		t.Fatalf("settled file should upload: %+v", r)
	}
	items, _ := a.s.C.List(context.Background(), 0, "")
	if len(items) != 1 || items[0].Name != "busy.mov" {
		t.Fatalf("server has %v", items)
	}
}
