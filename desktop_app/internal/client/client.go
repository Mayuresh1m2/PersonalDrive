// Package client is a small Go client for the PersonalDrive API
// (see shared_lib/api/openapi.yaml), used by the desktop sync agent.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

type File struct {
	ID      int64     `json:"id"`
	SpaceID *int64    `json:"space_id,omitempty"`
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	MTime   time.Time `json:"mtime"`
	SHA256  string    `json:"sha256"`
	Version int64     `json:"version"`
	State   string    `json:"state"`
}

type Change struct {
	Seq     int64  `json:"seq"`
	Op      string `json:"op"`
	FileID  int64  `json:"file_id"`
	SpaceID *int64 `json:"space_id,omitempty"`
	Path    string `json:"path"`
	OldPath string `json:"old_path"`
	File    *File  `json:"file"`
}

type Tokens struct {
	AccessToken  string    `json:"access_token"`
	AccessExp    time.Time `json:"access_expires_at"`
	RefreshToken string    `json:"refresh_token"`
	DeviceID     string    `json:"device_id"`
}

// Error is an API error.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message) }

// Retryable reports transient failures (network, server busy, disk unplugged).
func Retryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Status == 0 || e.Status == 429 || e.Status >= 500
	}
	return !errors.Is(err, context.Canceled)
}

func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
	// SaveTokens persists rotated tokens.
	SaveTokens func(Tokens) error

	mu     sync.Mutex
	tokens Tokens
}

// New builds a client. caPEM pins the server's private CA (nil: system roots,
// e.g. for plain-HTTP test servers).
func New(baseURL string, caPEM []byte, tokens Tokens) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("invalid CA certificate")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Transport: tr}, tokens: tokens}, nil
}

// FetchCA downloads the server CA and checks it against the fingerprint from
// the pairing QR code (trust on first use, verified).
func FetchCA(ctx context.Context, baseURL, wantSHA256 string) ([]byte, error) {
	insecure := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // verified by fingerprint below
	}}
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/v1/ca.pem", nil)
	resp, err := insecure.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	pemData, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, errors.New("server did not return a CA certificate")
	}
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("server did not return a CA certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	if got := hex.EncodeToString(sum[:]); got != wantSHA256 {
		return nil, fmt.Errorf("server CA fingerprint %s does not match the pairing code (%s)", got[:16], wantSHA256[:16])
	}
	return pemData, nil
}

func (c *Client) Tokens() Tokens {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens
}

func (c *Client) setTokens(t Tokens) error {
	c.mu.Lock()
	c.tokens = t
	c.mu.Unlock()
	if c.SaveTokens != nil {
		return c.SaveTokens(t)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string, auth bool) (*http.Response, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var buf []byte
	if body != nil {
		// Buffer so we can resend after a token refresh (bodies here are
		// JSON or one upload part, at most a few MiB).
		var err error
		if buf, err = io.ReadAll(body); err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		var rd io.Reader
		if buf != nil {
			rd = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if auth {
			if t := c.Tokens(); time.Until(t.AccessExp) < 30*time.Second && t.RefreshToken != "" {
				c.refresh(ctx)
			}
			req.Header.Set("Authorization", "Bearer "+c.Tokens().AccessToken)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &Error{Status: 0, Code: "network", Message: err.Error()}
		}
		if resp.StatusCode == 401 && auth && attempt == 0 {
			resp.Body.Close()
			if err := c.refresh(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			var e struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
			return nil, &Error{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
		}
		return resp, nil
	}
	return nil, &Error{Status: 401, Code: "unauthorized", Message: "session expired; pair this device again"}
}

func (c *Client) refresh(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"refresh_token": c.Tokens().RefreshToken})
	resp, err := c.do(ctx, "POST", "/api/v1/auth/refresh", nil, bytes.NewReader(body), "application/json", false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var t Tokens
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	return c.setTokens(t)
}

func (c *Client) json(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, query, body, ct, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == 204 {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- auth --------------------------------------------------------------------

func (c *Client) Login(ctx context.Context, user, pass, device string) error {
	return c.authCall(ctx, "/api/v1/auth/login", map[string]string{"username": user, "password": pass, "device_name": device, "platform": "desktop"})
}

func (c *Client) Pair(ctx context.Context, token, device string) error {
	return c.authCall(ctx, "/api/v1/devices/pair", map[string]string{"token": token, "device_name": device, "platform": "desktop"})
}

func (c *Client) authCall(ctx context.Context, path string, body map[string]string) error {
	b, _ := json.Marshal(body)
	resp, err := c.do(ctx, "POST", path, nil, bytes.NewReader(b), "application/json", false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var t Tokens
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	return c.setTokens(t)
}

// ---- files -------------------------------------------------------------------------

func spaceQuery(q url.Values, spaceID int64) url.Values {
	if spaceID > 0 {
		q.Set("space", strconv.FormatInt(spaceID, 10))
	}
	return q
}

// List returns all entries directly in dir.
func (c *Client) List(ctx context.Context, spaceID int64, dir string) ([]File, error) {
	var out []File
	after := ""
	for {
		var page struct {
			Items     []File `json:"items"`
			NextAfter string `json:"next_after"`
		}
		q := spaceQuery(url.Values{"path": {dir}, "limit": {"5000"}}, spaceID)
		if after != "" {
			q.Set("after", after)
		}
		if err := c.json(ctx, "GET", "/api/v1/files", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextAfter == "" {
			return out, nil
		}
		after = page.NextAfter
	}
}

// Download streams a file's content into w.
func (c *Client) Download(ctx context.Context, id int64, w io.Writer) error {
	resp, err := c.do(ctx, "GET", fmt.Sprintf("/api/v1/files/%d/content", id), nil, nil, "", true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (c *Client) Trash(ctx context.Context, id int64) error {
	return c.json(ctx, "DELETE", fmt.Sprintf("/api/v1/files/%d", id), nil, nil, nil)
}

func (c *Client) Mkdir(ctx context.Context, spaceID int64, path string) (*File, error) {
	var f File
	return &f, c.json(ctx, "POST", "/api/v1/folders", nil, map[string]any{"space_id": spaceID, "path": path}, &f)
}

func (c *Client) Cursor(ctx context.Context) (int64, error) {
	var r struct{ Cursor int64 }
	return r.Cursor, c.json(ctx, "GET", "/api/v1/sync/cursor", nil, nil, &r)
}

func (c *Client) Changes(ctx context.Context, cursor int64, wait int) ([]Change, int64, bool, error) {
	var r struct {
		Changes []Change `json:"changes"`
		Cursor  int64    `json:"cursor"`
		HasMore bool     `json:"has_more"`
	}
	q := url.Values{"cursor": {strconv.FormatInt(cursor, 10)}, "wait": {strconv.Itoa(wait)}}
	err := c.json(ctx, "GET", "/api/v1/sync/changes", q, nil, &r)
	return r.Changes, r.Cursor, r.HasMore, err
}

// UploadResult is the outcome of Upload.
type UploadResult struct {
	File         File `json:"file"`
	Deduplicated bool `json:"deduplicated"`
	Conflict     bool `json:"conflict"`
	Unchanged    bool `json:"unchanged"`
}

// Upload sends a local file with the resumable protocol. baseVersion 0 means
// "new file"; a stale baseVersion makes the server keep both copies.
func (c *Client) Upload(ctx context.Context, spaceID int64, remotePath, localPath, sha string, baseVersion int64) (*UploadResult, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	req := map[string]any{
		"space_id": spaceID, "path": remotePath, "size": st.Size(), "sha256": sha, "mtime": st.ModTime().UTC(),
	}
	if baseVersion > 0 {
		req["base_version"] = baseVersion
	}
	var start struct {
		UploadResult
		Status   string `json:"status"`
		UploadID string `json:"upload_id"`
		PartSize int64  `json:"part_size"`
		Parts    int    `json:"parts"`
	}
	if err := c.json(ctx, "POST", "/api/v1/uploads", nil, req, &start); err != nil {
		return nil, err
	}
	if start.Status == "completed" {
		return &start.UploadResult, nil
	}
	buf := make([]byte, start.PartSize)
	for n := 0; n < start.Parts; n++ {
		k, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		resp, err := c.do(ctx, "PUT", fmt.Sprintf("/api/v1/uploads/%s/parts/%d", start.UploadID, n), nil, bytes.NewReader(buf[:k]), "application/octet-stream", true)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
	}
	var done UploadResult
	if err := c.json(ctx, "POST", "/api/v1/uploads/"+start.UploadID+"/complete", nil, nil, &done); err != nil {
		return nil, err
	}
	return &done, nil
}
