package api

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
)

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.DB.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	type userWithUsage struct {
		*metadata.User
		UsedBytes int64 `json:"used_bytes"`
	}
	out := make([]userWithUsage, 0, len(users))
	for _, u := range users {
		used, _ := s.DB.UserUsage(r.Context(), u.ID)
		out = append(out, userWithUsage{u, used})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		Role        string `json:"role"`
		QuotaBytes  int64  `json:"quota_bytes"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, err := storage.CleanName(req.Username)
	if err != nil {
		s.fail(w, err)
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	u, err := s.Auth.CreateUser(r.Context(), name, req.DisplayName, req.Password, req.Role)
	if err != nil {
		s.fail(w, err)
		return
	}
	if req.QuotaBytes > 0 {
		u.QuotaBytes = req.QuotaBytes
		if err := s.DB.UpdateUser(r.Context(), u); err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.DB.UserByID(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	var req struct {
		DisplayName *string `json:"display_name"`
		Role        *string `json:"role"`
		QuotaBytes  *int64  `json:"quota_bytes"`
		Disabled    *bool   `json:"disabled"`
		Password    *string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.DisplayName != nil {
		u.DisplayName = *req.DisplayName
	}
	if req.Role != nil {
		if *req.Role != "admin" && *req.Role != "member" {
			writeErr(w, http.StatusBadRequest, "bad_request", "role must be admin or member")
			return
		}
		if u.ID == userFrom(r).ID && *req.Role != "admin" {
			writeErr(w, http.StatusBadRequest, "bad_request", "you can't remove your own admin role")
			return
		}
		u.Role = *req.Role
	}
	if req.QuotaBytes != nil {
		u.QuotaBytes = *req.QuotaBytes
	}
	if req.Disabled != nil {
		if u.ID == userFrom(r).ID && *req.Disabled {
			writeErr(w, http.StatusBadRequest, "bad_request", "you can't disable yourself")
			return
		}
		u.Disabled = *req.Disabled
	}
	if req.Password != nil {
		if err := s.Auth.SetPassword(r.Context(), u.ID, *req.Password); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.DB.UpdateUser(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) adminListSpaces(w http.ResponseWriter, r *http.Request) {
	spaces, err := s.DB.ListSpaces(r.Context(), 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(spaces)})
}

func (s *Server) adminCreateSpace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, err := storage.CleanName(req.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	sp, err := s.DB.CreateSpace(r.Context(), name, userFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The creator is a writer by default.
	if err := s.DB.SetSpaceMember(r.Context(), sp.ID, userFrom(r).ID, "write"); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sp)
}

func (s *Server) adminSpaceMembers(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		s.fail(w, err)
		return
	}
	ms, err := s.DB.SpaceMembers(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(ms)})
}

func (s *Server) adminSetMember(w http.ResponseWriter, r *http.Request) {
	spaceID, err := pathInt(r, "id")
	if err != nil {
		s.fail(w, err)
		return
	}
	userID, err := pathInt(r, "user")
	if err != nil {
		s.fail(w, err)
		return
	}
	var req struct {
		Permission string `json:"permission"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Permission != "read" && req.Permission != "write" {
		writeErr(w, http.StatusBadRequest, "bad_request", "permission must be read or write")
		return
	}
	if _, err := s.DB.SpaceByID(r.Context(), spaceID); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.DB.UserByID(r.Context(), userID); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.DB.SetSpaceMember(r.Context(), spaceID, userID, req.Permission); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminRemoveMember(w http.ResponseWriter, r *http.Request) {
	spaceID, err := pathInt(r, "id")
	if err != nil {
		s.fail(w, err)
		return
	}
	userID, err := pathInt(r, "user")
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.DB.RemoveSpaceMember(r.Context(), spaceID, userID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Health(r.Context()))
}

var reindexRunning atomic.Bool

// adminReindex starts a background reindex; progress goes to the server log.
func (s *Server) adminReindex(w http.ResponseWriter, r *http.Request) {
	if !reindexRunning.CompareAndSwap(false, true) {
		writeErr(w, http.StatusConflict, "busy", "a reindex is already running")
		return
	}
	go func() {
		defer reindexRunning.Store(false)
		rep, err := s.Lib.Reindex(context.Background(), func(msg string) { s.Log.Info("reindex", "progress", msg) })
		if err != nil {
			s.Log.Error("reindex failed", "err", err)
			return
		}
		s.Log.Info("reindex finished", "report", rep)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started"})
}
