package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/app"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

type env struct {
	t    *testing.T
	app  *app.App
	srv  *httptest.Server
	code string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "disk-a")
	os.MkdirAll(lib, 0o750)
	if _, err := volume.Init(lib, volume.RoleLibrary); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Paths.State = filepath.Join(dir, "state")
	cfg.Paths.Library = lib
	cfg.Server.TLS, cfg.Server.MDNS = false, false
	cfg.Uploads.PartSize = 1 << 20
	cfg.Auth.LoginRatePerMin = 1000
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.Open(cfg, log, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go a.Jobs.Run(ctx, 1)
	srv := httptest.NewServer(a.APIServer(nil, nil).Handler())
	t.Cleanup(func() { srv.Close(); cancel(); a.Close() })
	return &env{t: t, app: a, srv: srv, code: a.InitSetupCode(ctx)}
}

type client struct {
	e     *env
	token string
}

func (c *client) do(method, path string, body any, out any) int {
	c.e.t.Helper()
	var rd io.Reader
	if b, ok := body.([]byte); ok {
		rd = bytes.NewReader(b)
	} else if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, rd)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		if p, ok := out.(*[]byte); ok {
			*p = data
		} else if len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				c.e.t.Fatalf("%s %s: decoding %q: %v", method, path, data, err)
			}
		}
	}
	return resp.StatusCode
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	DeviceID     string `json:"device_id"`
}

type fileResp struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version int64  `json:"version"`
	Size    int64  `json:"size"`
}

type uploadResp struct {
	Status       string    `json:"status"`
	UploadID     string    `json:"upload_id"`
	PartSize     int64     `json:"part_size"`
	Parts        int       `json:"parts"`
	Received     []int     `json:"received"`
	File         *fileResp `json:"file"`
	Deduplicated bool      `json:"deduplicated"`
	Conflict     bool      `json:"conflict"`
}

func (e *env) admin() *client {
	c := &client{e: e}
	var tok tokens
	if code := c.do("POST", "/api/v1/setup", map[string]any{
		"setup_code": e.code, "username": "alice", "password": "alice's long password", "device_name": "laptop",
	}, &tok); code != 201 {
		e.t.Fatalf("setup: %d", code)
	}
	c.token = tok.AccessToken
	return c
}

func (e *env) member(admin *client, name string) *client {
	if code := admin.do("POST", "/api/v1/admin/users", map[string]any{"username": name, "password": name + " long password"}, nil); code != 201 {
		e.t.Fatalf("create user: %d", code)
	}
	c := &client{e: e}
	var tok tokens
	if code := c.do("POST", "/api/v1/auth/login", map[string]any{"username": name, "password": name + " long password", "device_name": name + "-phone"}, &tok); code != 200 {
		e.t.Fatalf("login: %d", code)
	}
	c.token = tok.AccessToken
	return c
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// upload runs the full resumable upload protocol and returns the final response.
func (c *client) upload(spaceID int64, path string, content []byte, base *int64) (uploadResp, int) {
	var start uploadResp
	code := c.do("POST", "/api/v1/uploads", map[string]any{
		"space_id": spaceID, "path": path, "size": len(content), "sha256": sum(content),
		"mtime": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), "base_version": base,
	}, &start)
	if code >= 300 || start.Status == "completed" {
		return start, code
	}
	for n := 0; n < start.Parts; n++ {
		lo := int64(n) * start.PartSize
		hi := min(lo+start.PartSize, int64(len(content)))
		if code := c.do("PUT", fmt.Sprintf("/api/v1/uploads/%s/parts/%d", start.UploadID, n), content[lo:hi], nil); code != 204 {
			c.e.t.Fatalf("part %d: %d", n, code)
		}
	}
	var done uploadResp
	code = c.do("POST", "/api/v1/uploads/"+start.UploadID+"/complete", nil, &done)
	return done, code
}

func TestSetupRequiresCode(t *testing.T) {
	e := newEnv(t)
	c := &client{e: e}
	if code := c.do("POST", "/api/v1/setup", map[string]any{"setup_code": "WRONG", "username": "x", "password": "long enough password"}, nil); code != 403 {
		t.Fatalf("expected 403, got %d", code)
	}
	e.admin()
	if code := c.do("POST", "/api/v1/setup", map[string]any{"setup_code": e.code, "username": "y", "password": "long enough password"}, nil); code != 409 {
		t.Fatalf("second setup should be refused, got %d", code)
	}
}

func TestUploadDownloadDedupAndIsolation(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	bob := e.member(alice, "bob")

	content := make([]byte, 2<<20+12345) // 3 parts at 1 MiB
	rand.Read(content)
	res, code := alice.upload(0, "Photos/big.raw", content, nil)
	if code != 200 || res.Status != "completed" || res.File.Size != int64(len(content)) {
		t.Fatalf("upload: %d %+v", code, res)
	}

	var got []byte
	if code := alice.do("GET", fmt.Sprintf("/api/v1/files/%d/content", res.File.ID), nil, &got); code != 200 || !bytes.Equal(got, content) {
		t.Fatalf("download mismatch: %d", code)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/files/%d/content", e.srv.URL, res.File.ID), nil)
	req.Header.Set("Authorization", "Bearer "+alice.token)
	req.Header.Set("Range", "bytes=10-19")
	resp, _ := http.DefaultClient.Do(req)
	part, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || !bytes.Equal(part, content[10:20]) {
		t.Fatalf("range request: %d", resp.StatusCode)
	}

	// Bob uploads the same bytes: no transfer needed (household dedup).
	var start uploadResp
	bob.do("POST", "/api/v1/uploads", map[string]any{"path": "copy.raw", "size": len(content), "sha256": sum(content)}, &start)
	if start.Status != "completed" || !start.Deduplicated {
		t.Fatalf("expected instant dedup, got %+v", start)
	}

	// Bob can't see Alice's file.
	if code := bob.do("GET", fmt.Sprintf("/api/v1/files/%d", res.File.ID), nil, nil); code != 404 {
		t.Fatalf("bob read alice's file: %d", code)
	}

	// Corrupted upload is rejected.
	bad := []byte("hello world")
	var s2 uploadResp
	alice.do("POST", "/api/v1/uploads", map[string]any{"path": "bad.txt", "size": len(bad), "sha256": sum([]byte("other"))}, &s2)
	alice.do("PUT", "/api/v1/uploads/"+s2.UploadID+"/parts/0", bad, nil)
	if code := alice.do("POST", "/api/v1/uploads/"+s2.UploadID+"/complete", nil, nil); code != 422 {
		t.Fatalf("expected hash mismatch 422, got %d", code)
	}
}

func TestResumeUpload(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	content := make([]byte, 3<<20)
	rand.Read(content)
	var start uploadResp
	alice.do("POST", "/api/v1/uploads", map[string]any{"path": "video.mp4", "size": len(content), "sha256": sum(content)}, &start)
	alice.do("PUT", "/api/v1/uploads/"+start.UploadID+"/parts/1", content[1<<20:2<<20], nil)
	if code := alice.do("POST", "/api/v1/uploads/"+start.UploadID+"/complete", nil, nil); code != 409 {
		t.Fatalf("incomplete upload should be refused, got %d", code)
	}
	var status uploadResp
	alice.do("GET", "/api/v1/uploads/"+start.UploadID, nil, &status)
	if len(status.Received) != 1 || status.Received[0] != 1 {
		t.Fatalf("received parts: %v", status.Received)
	}
	if code := alice.do("PUT", "/api/v1/uploads/"+start.UploadID+"/parts/0", content[:100], nil); code != 400 {
		t.Fatalf("short part should be rejected, got %d", code)
	}
	alice.do("PUT", "/api/v1/uploads/"+start.UploadID+"/parts/0", content[:1<<20], nil)
	alice.do("PUT", "/api/v1/uploads/"+start.UploadID+"/parts/2", content[2<<20:], nil)
	var done uploadResp
	if code := alice.do("POST", "/api/v1/uploads/"+start.UploadID+"/complete", nil, &done); code != 200 || done.File == nil {
		t.Fatalf("complete after resume: %d", code)
	}
}

func TestConflictViaAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	first, _ := alice.upload(0, "notes.txt", []byte("v1"), nil)
	v := first.File.Version
	second, _ := alice.upload(0, "notes.txt", []byte("v2 from laptop"), &v)
	if second.Conflict || second.File.Version != 2 {
		t.Fatalf("expected clean update, got %+v", second)
	}
	third, _ := alice.upload(0, "notes.txt", []byte("v2 from phone"), &v) // stale base
	if !third.Conflict {
		t.Fatalf("expected conflict, got %+v", third)
	}
}

func TestChangesFeedAndLongPoll(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	var cur struct{ Cursor int64 }
	alice.do("GET", "/api/v1/sync/cursor", nil, &cur)
	alice.upload(0, "a.txt", []byte("a"), nil)

	var feed struct {
		Changes []struct {
			Seq  int64
			Op   string
			Path string
		}
		Cursor int64
	}
	alice.do("GET", fmt.Sprintf("/api/v1/sync/changes?cursor=%d", cur.Cursor), nil, &feed)
	if len(feed.Changes) != 1 || feed.Changes[0].Op != "create" || feed.Changes[0].Path != "a.txt" {
		t.Fatalf("unexpected feed %+v", feed)
	}

	// A long-poll returns as soon as something changes.
	done := make(chan time.Duration)
	go func() {
		start := time.Now()
		var f2 struct{ Changes []any }
		alice.do("GET", fmt.Sprintf("/api/v1/sync/changes?cursor=%d&wait=20", feed.Cursor), nil, &f2)
		done <- time.Since(start)
	}()
	time.Sleep(300 * time.Millisecond)
	alice.upload(0, "b.txt", []byte("b"), nil)
	select {
	case d := <-done:
		if d > 5*time.Second {
			t.Fatalf("long-poll too slow: %v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long-poll did not wake")
	}

	// Bob's feed doesn't include Alice's changes.
	bob := e.member(alice, "bob")
	var bf struct{ Changes []any }
	bob.do("GET", "/api/v1/sync/changes?cursor=0", nil, &bf)
	if len(bf.Changes) != 0 {
		t.Fatalf("bob sees alice's changes: %d", len(bf.Changes))
	}
}

func TestSpacesViaAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	bob := e.member(alice, "bob")
	var sp struct{ ID int64 }
	if code := alice.do("POST", "/api/v1/admin/spaces", map[string]any{"name": "Family Photos"}, &sp); code != 201 {
		t.Fatalf("create space: %d", code)
	}
	var users struct {
		Items []struct {
			ID       int64
			Username string
		}
	}
	alice.do("GET", "/api/v1/admin/users", nil, &users)
	var bobID int64
	for _, u := range users.Items {
		if u.Username == "bob" {
			bobID = u.ID
		}
	}
	alice.do("PUT", fmt.Sprintf("/api/v1/admin/spaces/%d/members/%d", sp.ID, bobID), map[string]any{"permission": "read"}, nil)
	if _, code := alice.upload(sp.ID, "trip/1.jpg", []byte("pic"), nil); code != 200 {
		t.Fatalf("alice upload to space: %d", code)
	}
	if _, code := bob.upload(sp.ID, "trip/2.jpg", []byte("pic2"), nil); code != 403 {
		t.Fatalf("read-only bob uploaded: %d", code)
	}
	var list struct{ Items []fileResp }
	bob.do("GET", fmt.Sprintf("/api/v1/files?space=%d&path=trip", sp.ID), nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("bob should see the space contents, got %d", len(list.Items))
	}
	if code := bob.do("POST", "/api/v1/admin/spaces", map[string]any{"name": "x"}, nil); code != 403 {
		t.Fatalf("member used admin API: %d", code)
	}
}

func TestReadOnlyWhenDiskUnplugged(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	os.Remove(filepath.Join(e.app.Cfg.Paths.Library, volume.MarkerName))
	e.app.Guard.Refresh()
	var errResp struct{ Error struct{ Code string } }
	code := alice.do("POST", "/api/v1/uploads", map[string]any{"path": "x", "size": 1, "sha256": sum([]byte("x"))}, &errResp)
	if code != 503 || errResp.Error.Code != "library_unavailable" {
		t.Fatalf("expected 503 library_unavailable, got %d %+v", code, errResp)
	}
	var h struct {
		ReadOnly bool `json:"read_only"`
	}
	alice.do("GET", "/api/v1/admin/health", nil, &h)
	if !h.ReadOnly {
		t.Fatal("health should report read-only")
	}
}

func TestPairingAndRefresh(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	var pt struct {
		Token   string
		Payload string
		QRPNG   string `json:"qr_png"`
	}
	if code := alice.do("POST", "/api/v1/devices/pairing-tokens", nil, &pt); code != 201 || pt.QRPNG == "" {
		t.Fatalf("pairing token: %d", code)
	}
	phone := &client{e: e}
	var tok tokens
	if code := phone.do("POST", "/api/v1/devices/pair", map[string]any{"token": pt.Token, "device_name": "Pixel"}, &tok); code != 200 {
		t.Fatalf("pair: %d", code)
	}
	var tok2 tokens
	if code := phone.do("POST", "/api/v1/auth/refresh", map[string]any{"refresh_token": tok.RefreshToken}, &tok2); code != 200 {
		t.Fatalf("refresh: %d", code)
	}
	phone.token = tok2.AccessToken
	if code := phone.do("GET", "/api/v1/me", nil, nil); code != 200 {
		t.Fatalf("me with refreshed token: %d", code)
	}
	// Revoking the device kills its tokens.
	alice.do("DELETE", "/api/v1/devices/"+tok.DeviceID, nil, nil)
	if code := phone.do("GET", "/api/v1/me", nil, nil); code != 401 {
		t.Fatalf("revoked device still works: %d", code)
	}
}

func TestThumbnailGenerated(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for x := 0; x < 800; x++ {
		img.Set(x, x%600, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	res, _ := alice.upload(0, "pic.png", buf.Bytes(), nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var data []byte
		code := alice.do("GET", fmt.Sprintf("/api/v1/files/%d/thumbnail?size=200", res.File.ID), nil, &data)
		if code == 200 {
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || cfg.Width != 256 || cfg.Height != 192 {
				t.Fatalf("thumbnail %dx%d %v", cfg.Width, cfg.Height, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("thumbnail never generated (last code %d)", code)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for {
		var f struct {
			Media *struct{ Width, Height int }
		}
		alice.do("GET", fmt.Sprintf("/api/v1/files/%d", res.File.ID), nil, &f)
		if f.Media != nil {
			if f.Media.Width != 800 || f.Media.Height != 600 {
				t.Fatalf("media info wrong: %+v", f.Media)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("media info never extracted")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestWebUIAndMediaTokens(t *testing.T) {
	e := newEnv(t)
	alice := e.admin()
	for _, p := range []string{"/", "/space/3", "/admin/people"} {
		resp, err := http.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte("PersonalDrive")) {
			t.Fatalf("GET %s: %d", p, resp.StatusCode)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Fatalf("missing CSP on %s: %q", p, csp)
		}
	}
	if resp, _ := http.Get(e.srv.URL + "/assets/missing.js"); resp.StatusCode != 404 {
		t.Fatalf("missing asset should 404, got %d", resp.StatusCode)
	}

	res, _ := alice.upload(0, "page.html", []byte("<script>alert(1)</script>"), nil)
	var tok tokens
	(&client{e: e}).do("POST", "/api/v1/auth/login", map[string]any{"username": "alice", "password": "alice's long password"}, &tok)
	content := fmt.Sprintf("%s/api/v1/files/%d/content?access_token=%s", e.srv.URL, res.File.ID, tok.AccessToken)
	resp, err := http.Get(content)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("query token on content: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox") {
		t.Fatalf("user HTML must be sandboxed, CSP = %q", csp)
	}
	// Query tokens are not accepted on other endpoints.
	if resp, _ := http.Get(fmt.Sprintf("%s/api/v1/me?access_token=%s", e.srv.URL, tok.AccessToken)); resp.StatusCode != 401 {
		t.Fatalf("query token accepted on /me: %d", resp.StatusCode)
	}
}
