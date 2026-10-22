// Package api implements the HTTP API described in shared_lib/api/openapi.yaml.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/auth"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/events"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/health"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/jobs"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/library"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/processors"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/tlsutil"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

type Deps struct {
	Cfg      *config.Config
	DB       *metadata.DB
	Lib      *library.Service
	Auth     *auth.Service
	Notifier *events.Notifier
	Jobs     *jobs.Queue
	Thumbs   *processors.Thumbnails
	TLS      *tlsutil.Manager // nil when TLS is disabled
	Health   func(context.Context) health.Report
	Log      *slog.Logger
	Version  string
	// SetupCode returns the one-time code required to create the first admin
	// ("" once setup is done).
	SetupCode func() string
	// Extra lets later phases (backup) register more routes on the mux.
	Extra func(mux *http.ServeMux, s *Server)
	// WebUI serves the web app for every non-API path (nil: none).
	WebUI http.Handler
}

type Server struct {
	Deps
	limiter *ipLimiter
}

var (
	reqCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pdrive_http_requests_total", Help: "HTTP requests by route and status.",
	}, []string{"route", "code"})
	reqDur = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "pdrive_http_request_duration_seconds", Help: "HTTP request latency.",
		Buckets: prometheus.ExponentialBuckets(0.005, 4, 8),
	}, []string{"route"})
)

func New(d Deps) *Server {
	return &Server{Deps: d, limiter: newIPLimiter(rate.Limit(float64(d.Cfg.Auth.LoginRatePerMin)/60), d.Cfg.Auth.LoginRatePerMin)}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	pub := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	authed := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h, false)) }
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h, true)) }

	pub("GET /healthz", s.healthz)
	pub("GET /api/v1/server", s.serverInfo)
	pub("GET /api/v1/ca.pem", s.caPEM)
	pub("POST /api/v1/setup", s.limited(s.setup))
	pub("POST /api/v1/auth/login", s.limited(s.login))
	pub("POST /api/v1/auth/refresh", s.limited(s.refresh))
	pub("POST /api/v1/devices/pair", s.limited(s.pair))
	if s.Cfg.Server.Metrics {
		mux.Handle("GET /metrics", promhttp.Handler())
	}

	authed("POST /api/v1/auth/logout", s.logout)
	authed("GET /api/v1/me", s.me)
	authed("GET /api/v1/devices", s.listDevices)
	authed("DELETE /api/v1/devices/{id}", s.revokeDevice)
	authed("POST /api/v1/devices/pairing-tokens", s.createPairingToken)
	authed("GET /api/v1/spaces", s.mySpaces)

	authed("GET /api/v1/files", s.listFiles)
	authed("GET /api/v1/files/{id}", s.getFile)
	// <img>/<video> can't send headers, so these also accept ?access_token=.
	mux.Handle("GET /api/v1/files/{id}/content", s.requireAuthQuery(s.fileContent))
	mux.Handle("GET /api/v1/files/{id}/thumbnail", s.requireAuthQuery(s.thumbnail))
	authed("PATCH /api/v1/files/{id}", s.patchFile)
	authed("DELETE /api/v1/files/{id}", s.deleteFile)
	authed("POST /api/v1/folders", s.createFolder)
	authed("GET /api/v1/trash", s.listTrash)
	authed("POST /api/v1/trash/{id}/restore", s.restoreTrash)
	authed("DELETE /api/v1/trash/{id}", s.purgeTrash)

	authed("POST /api/v1/uploads", s.startUpload)
	authed("GET /api/v1/uploads/{id}", s.uploadStatus)
	authed("PUT /api/v1/uploads/{id}/parts/{n}", s.uploadPart)
	authed("POST /api/v1/uploads/{id}/complete", s.completeUpload)
	authed("DELETE /api/v1/uploads/{id}", s.abortUpload)

	authed("POST /api/v1/sync/have", s.have)
	authed("GET /api/v1/sync/changes", s.changes)
	authed("GET /api/v1/sync/cursor", s.cursor)
	mux.Handle("GET /api/v1/events", s.requireAuthQuery(s.events))

	admin("GET /api/v1/admin/users", s.adminListUsers)
	admin("POST /api/v1/admin/users", s.adminCreateUser)
	admin("PATCH /api/v1/admin/users/{id}", s.adminUpdateUser)
	admin("GET /api/v1/admin/spaces", s.adminListSpaces)
	admin("POST /api/v1/admin/spaces", s.adminCreateSpace)
	admin("GET /api/v1/admin/spaces/{id}/members", s.adminSpaceMembers)
	admin("PUT /api/v1/admin/spaces/{id}/members/{user}", s.adminSetMember)
	admin("DELETE /api/v1/admin/spaces/{id}/members/{user}", s.adminRemoveMember)
	admin("GET /api/v1/admin/health", s.adminHealth)
	admin("POST /api/v1/admin/reindex", s.adminReindex)

	if s.Extra != nil {
		s.Extra(mux, s)
	}
	if s.WebUI != nil {
		mux.Handle("GET /", s.WebUI)
	}
	return s.instrument(mux)
}

// AuthedHandler wraps h with authentication (for routes added via Extra).
func (s *Server) AuthedHandler(h http.HandlerFunc, adminOnly bool) http.Handler {
	return s.requireAuth(h, adminOnly)
}

// ---- Middleware -----------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int)           { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				writeErr(sw, http.StatusInternalServerError, "internal", "internal error")
			}
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			reqCount.WithLabelValues(route, strconv.Itoa(sw.code)).Inc()
			reqDur.WithLabelValues(route).Observe(time.Since(start).Seconds())
			if sw.code >= 500 {
				s.Log.Warn("request failed", "route", route, "code", sw.code, "dur", time.Since(start))
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(sw, r)
	})
}

type ctxKey int

const (
	userKey ctxKey = iota
	deviceKey
)

func (s *Server) authenticate(r *http.Request, token string) (*metadata.User, *metadata.Device, error) {
	claims, err := s.Auth.Verify(token)
	if err != nil {
		return nil, nil, err
	}
	u, err := s.DB.UserByID(r.Context(), claims.UserID)
	if err != nil || u.Disabled {
		return nil, nil, auth.ErrInvalidToken
	}
	d, err := s.DB.DeviceByID(r.Context(), claims.DeviceID)
	if err != nil || d.RevokedAt != nil || d.UserID != u.ID {
		return nil, nil, auth.ErrInvalidToken
	}
	return u, d, nil
}

func (s *Server) requireAuth(h http.HandlerFunc, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		u, d, err := s.authenticate(r, tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		}
		if adminOnly && !u.IsAdmin() {
			writeErr(w, http.StatusForbidden, "forbidden", "admin only")
			return
		}
		ctx := context.WithValue(context.WithValue(r.Context(), userKey, u), deviceKey, d)
		h(w, r.WithContext(ctx))
	})
}

// requireAuthQuery also accepts ?access_token= because browsers can't set
// headers on WebSocket connections or <img>/<video> requests. Only used for
// read-only GET endpoints; access tokens expire after 15 minutes.
func (s *Server) requireAuthQuery(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.URL.Query().Get("access_token") != "" {
			r.Header.Set("Authorization", "Bearer "+r.URL.Query().Get("access_token"))
		}
		s.requireAuth(h, false).ServeHTTP(w, r)
	})
}

func userFrom(r *http.Request) *metadata.User { return r.Context().Value(userKey).(*metadata.User) }
func deviceFrom(r *http.Request) *metadata.Device {
	return r.Context().Value(deviceKey).(*metadata.Device)
}

// limited applies the per-IP rate limit used for credential endpoints.
func (s *Server) limited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again in a minute")
			return
		}
		h(w, r)
	}
}

type ipLimiter struct {
	mu    sync.Mutex
	r     rate.Limit
	burst int
	m     map[string]*limiterEntry
}

type limiterEntry struct {
	l    *rate.Limiter
	seen time.Time
}

func newIPLimiter(r rate.Limit, burst int) *ipLimiter {
	return &ipLimiter{r: r, burst: max(burst, 1), m: map[string]*limiterEntry{}}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 10000 {
		for k, e := range l.m {
			if time.Since(e.seen) > 10*time.Minute {
				delete(l.m, k)
			}
		}
	}
	e, ok := l.m[ip]
	if !ok {
		e = &limiterEntry{l: rate.NewLimiter(l.r, l.burst)}
		l.m[ip] = e
	}
	e.seen = time.Now()
	return e.l.Allow()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- JSON helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message = errCode, msg
	writeJSON(w, code, e)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// fail maps service errors onto HTTP responses.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, metadata.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, library.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", "you don't have write access here")
	case errors.Is(err, volume.ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "library_unavailable", err.Error())
	case errors.Is(err, storage.ErrInvalidPath):
		writeErr(w, http.StatusBadRequest, "invalid_path", err.Error())
	case errors.Is(err, library.ErrIsDir), errors.Is(err, library.ErrNotDir),
		errors.Is(err, library.ErrExists), errors.Is(err, metadata.ErrDuplicate):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, library.ErrHashMismatch):
		writeErr(w, http.StatusUnprocessableEntity, "hash_mismatch", err.Error())
	case errors.Is(err, library.ErrQuota):
		writeErr(w, http.StatusInsufficientStorage, "quota_exceeded", err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrInvalidToken):
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
	case errors.Is(err, auth.ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, context.Canceled):
		writeErr(w, 499, "canceled", "request canceled")
	default:
		s.Log.Error("internal error", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func pathInt(r *http.Request, name string) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad %s", metadata.ErrNotFound, name)
	}
	return v, nil
}

func queryInt(r *http.Request, name string, def int64) int64 {
	v, err := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	if err != nil {
		return def
	}
	return v
}
