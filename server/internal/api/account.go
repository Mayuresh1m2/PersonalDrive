package api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/tlsutil"
)

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "read_only": s.Lib.Guard().Writable() != nil})
}

// CAFingerprint is the pinned CA hash clients verify ("" without TLS).
func (s *Server) CAFingerprint() string {
	if s.TLS == nil {
		return ""
	}
	return s.TLS.CAFingerprint()
}

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":           s.Cfg.Server.Name,
		"version":        s.Version,
		"ca_sha256":      s.CAFingerprint(),
		"setup_required": s.SetupCode() != "",
		"read_only":      s.Lib.Guard().Writable() != nil,
		"dedup_scope":    s.Cfg.Dedup.Scope,
		"part_size":      s.Cfg.Uploads.PartSize,
	})
}

func (s *Server) caPEM(w http.ResponseWriter, r *http.Request) {
	if s.TLS == nil {
		writeErr(w, http.StatusNotFound, "not_found", "TLS is disabled")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="pdrive-ca.pem"`)
	w.Write(s.TLS.CAPEM())
}

// URLs returns the addresses clients should try, best first.
func (s *Server) URLs() []string {
	if s.Cfg.Server.PublicURL != "" {
		return []string{s.Cfg.Server.PublicURL}
	}
	scheme := "http"
	if s.Cfg.Server.TLS {
		scheme = "https"
	}
	_, port, err := net.SplitHostPort(s.Cfg.Server.Listen)
	if err != nil {
		port = "8443"
	}
	var out []string
	if host := s.Cfg.Server.Name; host != "" {
		out = append(out, fmt.Sprintf("%s://%s.local:%s", scheme, host, port))
	}
	for _, ip := range tlsutil.LANAddresses() {
		out = append(out, fmt.Sprintf("%s://%s:%s", scheme, ip, port))
	}
	return out
}

type setupReq struct {
	SetupCode   string `json:"setup_code"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	DeviceName  string `json:"device_name"`
	Platform    string `json:"platform"`
}

// setup creates the first admin. It requires the one-time code printed in the
// server log so a stranger on the Wi-Fi can't claim the server first.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req setupReq
	if !readJSON(w, r, &req) {
		return
	}
	code := s.SetupCode()
	if code == "" {
		writeErr(w, http.StatusConflict, "already_setup", "server already has users")
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToUpper(strings.TrimSpace(req.SetupCode))), []byte(code)) != 1 {
		writeErr(w, http.StatusForbidden, "bad_setup_code", "setup code does not match the one in the server log")
		return
	}
	name, err := storage.CleanName(req.Username)
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.Auth.CreateUser(r.Context(), name, req.DisplayName, req.Password, "admin"); err != nil {
		s.fail(w, err)
		return
	}
	tok, err := s.Auth.Login(r.Context(), name, req.Password, req.DeviceName, req.Platform)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

type loginReq struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !readJSON(w, r, &req) {
		return
	}
	tok, err := s.Auth.Login(r.Context(), req.Username, req.Password, req.DeviceName, req.Platform)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tok)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tok, err := s.Auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tok)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.RevokeDevice(r.Context(), userFrom(r).ID, deviceFrom(r).ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token      string `json:"token"`
		DeviceName string `json:"device_name"`
		Platform   string `json:"platform"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tok, err := s.Auth.Pair(r.Context(), req.Token, req.DeviceName, req.Platform)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tok)
}

// PairingPayload is encoded in the QR code a new device scans.
type PairingPayload struct {
	V        int      `json:"v"`
	Name     string   `json:"name"`
	URLs     []string `json:"urls"`
	CASHA256 string   `json:"ca_sha256,omitempty"`
	Token    string   `json:"token"`
}

func (s *Server) createPairingToken(w http.ResponseWriter, r *http.Request) {
	tok, exp, err := s.Auth.CreatePairingToken(r.Context(), userFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	payload, _ := json.Marshal(PairingPayload{V: 1, Name: s.Cfg.Server.Name, URLs: s.URLs(), CASHA256: s.CAFingerprint(), Token: tok})
	png, err := qrcode.Encode(string(payload), qrcode.Medium, 512)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      tok,
		"expires_at": exp.UTC(),
		"payload":    string(payload),
		"qr_png":     base64.StdEncoding.EncodeToString(png),
	})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	used, err := s.DB.UserUsage(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	spaces, err := s.DB.ListSpaces(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": u, "device": deviceFrom(r), "used_bytes": used, "spaces": nonNil(spaces),
	})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := s.DB.ListDevices(r.Context(), userFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(ds)})
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.RevokeDevice(r.Context(), userFrom(r).ID, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mySpaces(w http.ResponseWriter, r *http.Request) {
	spaces, err := s.DB.ListSpaces(r.Context(), userFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(spaces)})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
