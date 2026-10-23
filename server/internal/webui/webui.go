// Package webui serves the web app (web_app/, built into dist/ by `make web`)
// from the server binary itself, so it's same-origin with the API.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// csp allows only same-origin scripts/styles (plus WebAssembly compilation,
// used for fast SHA-256 of uploads); blob:/data: images for previews and QR
// codes. Inline styles are used by React style props.
const csp = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; img-src 'self' blob: data:; media-src 'self' blob:; " +
	"style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

const placeholder = `<!doctype html><meta charset="utf-8"><title>PersonalDrive</title>
<body style="font:16px system-ui;max-width:40em;margin:4em auto;padding:0 1em">
<h1>PersonalDrive server is running</h1>
<p>The web app isn't built into this binary. Build it with <code>make -C server build-full</code>,
or use the mobile/desktop apps. The API is at <code>/api/v1</code>.</p>`

// Handler serves static assets and falls back to index.html for app routes.
func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		index = []byte(placeholder)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(sub, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable") // content-hashed names
				}
				files.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(p, "assets/") || path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}
