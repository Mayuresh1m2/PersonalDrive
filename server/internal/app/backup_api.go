package app

import (
	"encoding/json"
	"net/http"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/api"
)

// registerBackupAPI adds the admin backup endpoints.
func (a *App) registerBackupAPI(mux *http.ServeMux, s *api.Server) {
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	writeErr := func(w http.ResponseWriter, code int, errCode, msg string) {
		writeJSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
	}

	mux.Handle("GET /api/v1/admin/backup/status", s.AuthedHandler(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"targets": a.Backup.Status(), "warnings": a.Backup.Warnings()})
	}, true))

	mux.Handle("POST /api/v1/admin/backup/run", s.AuthedHandler(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target string `json:"target"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		if err := a.Backup.RunAsync(req.Target); err != nil {
			writeErr(w, http.StatusConflict, "busy", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "target": req.Target})
	}, true))

	mux.Handle("GET /api/v1/admin/backup/snapshots", s.AuthedHandler(func(w http.ResponseWriter, r *http.Request) {
		rp, _, err := a.Backup.OpenRepo(r.Context(), r.URL.Query().Get("target"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "backup_unavailable", err.Error())
			return
		}
		snaps, err := rp.Snapshots(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, "backup_unavailable", err.Error())
			return
		}
		type item struct {
			ID    string `json:"id"`
			Time  any    `json:"time"`
			Host  string `json:"host"`
			Stats any    `json:"stats"`
		}
		out := make([]item, 0, len(snaps))
		for i := len(snaps) - 1; i >= 0; i-- {
			out = append(out, item{ID: snaps[i].ID, Time: snaps[i].Time, Host: snaps[i].Host, Stats: snaps[i].Stats})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}, true))
}
