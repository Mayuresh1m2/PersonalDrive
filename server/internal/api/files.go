package api

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/library"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/processors"
)

// FileJSON is the wire representation of a file or directory.
type FileJSON struct {
	ID          int64               `json:"id"`
	SpaceID     *int64              `json:"space_id,omitempty"`
	Path        string              `json:"path"`
	Name        string              `json:"name"`
	IsDir       bool                `json:"is_dir"`
	Size        int64               `json:"size"`
	MTime       time.Time           `json:"mtime"`
	SHA256      string              `json:"sha256,omitempty"`
	Mime        string              `json:"mime,omitempty"`
	Version     int64               `json:"version"`
	Seq         int64               `json:"seq"`
	State       string              `json:"state"`
	TrashReason string              `json:"trash_reason,omitempty"`
	TrashedAt   *time.Time          `json:"trashed_at,omitempty"`
	Media       *metadata.MediaInfo `json:"media,omitempty"`
}

func toJSON(f *metadata.File) FileJSON {
	j := FileJSON{
		ID: f.ID, Path: f.Path, Name: f.Name, IsDir: f.IsDir, Size: f.Size, MTime: f.MTime,
		SHA256: f.SHA256, Mime: f.Mime, Version: f.Version, Seq: f.Seq, State: f.State,
		TrashReason: f.TrashReason, TrashedAt: f.TrashedAt,
	}
	if f.Owner.Kind == "space" {
		id := f.Owner.ID
		j.SpaceID = &id
	}
	return j
}

func toJSONList(fs []*metadata.File) []FileJSON {
	out := make([]FileJSON, 0, len(fs))
	for _, f := range fs {
		out = append(out, toJSON(f))
	}
	return out
}

func (s *Server) access(r *http.Request, spaceID int64) (library.Access, error) {
	return s.Lib.Resolve(r.Context(), userFrom(r), spaceID)
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	a, err := s.access(r, queryInt(r, "space", 0))
	if err != nil {
		s.fail(w, err)
		return
	}
	limit := int(min(max(queryInt(r, "limit", 500), 1), 5000))
	items, err := s.Lib.List(r.Context(), a, r.URL.Query().Get("path"), r.URL.Query().Get("after"), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := map[string]any{"items": toJSONList(items)}
	if len(items) == limit {
		resp["next_after"] = items[len(items)-1].Name
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) fileFromPath(w http.ResponseWriter, r *http.Request) (*metadata.File, library.Access, bool) {
	id, err := pathInt(r, "id")
	if err != nil {
		s.fail(w, err)
		return nil, library.Access{}, false
	}
	f, a, err := s.Lib.FileForUser(r.Context(), userFrom(r), id)
	if err != nil {
		s.fail(w, err)
		return nil, library.Access{}, false
	}
	return f, a, true
}

func processorsInfo(f *metadata.File) processors.FileInfo {
	return processors.FileInfo{SHA256: f.SHA256, Mime: f.Mime, Name: f.Name, Size: f.Size}
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	f, _, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	j := toJSON(f)
	if f.SHA256 != "" {
		if m, err := s.DB.MediaInfo(r.Context(), f.SHA256); err == nil {
			j.Media = m
		}
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	f, a, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	fh, err := s.Lib.Open(r.Context(), a, f)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("%w: file missing on disk", metadata.ErrNotFound)
		}
		s.fail(w, err)
		return
	}
	defer fh.Close()
	w.Header().Set("ETag", `"`+f.SHA256+`"`)
	w.Header().Set("Content-Type", f.Mime)
	// User content is served from the app's origin: sandbox it so an uploaded
	// HTML/SVG file can't run scripts with the viewer's session. (Browsers
	// refuse to render PDFs in a sandbox, and PDFs can't reach the page.)
	if f.Mime != "application/pdf" {
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	disp := "inline"
	if r.URL.Query().Get("download") == "1" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": f.Name}))
	http.ServeContent(w, r, "", f.MTime, fh)
}

func (s *Server) thumbnail(w http.ResponseWriter, r *http.Request) {
	f, _, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	if f.SHA256 == "" || s.Thumbs == nil {
		writeErr(w, http.StatusNotFound, "thumbnail_unavailable", "no thumbnail for this item")
		return
	}
	size := s.Thumbs.NearestSize(int(queryInt(r, "size", 256)))
	p := s.Thumbs.Path(f.SHA256, size)
	if _, err := os.Stat(p); err != nil {
		code := "thumbnail_pending"
		if st, err := s.DB.ProcessorState(r.Context(), s.Thumbs.Name(), f.SHA256); err == nil && st.Status != "done" {
			code = "thumbnail_unavailable"
		} else if !s.Thumbs.Accepts(processorsInfo(f)) {
			code = "thumbnail_unavailable"
		}
		writeErr(w, http.StatusNotFound, code, "thumbnail not available")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, p)
}

func (s *Server) patchFile(w http.ResponseWriter, r *http.Request) {
	f, a, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	nf, err := s.Lib.Move(r.Context(), a, f, req.Path, deviceFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toJSON(nf))
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	f, a, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	if err := s.Lib.Trash(r.Context(), a, f); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SpaceID int64  `json:"space_id"`
		Path    string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	a, err := s.access(r, req.SpaceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	f, err := s.Lib.Mkdir(r.Context(), a, req.Path, deviceFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toJSON(f))
}

func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	a, err := s.access(r, queryInt(r, "space", 0))
	if err != nil {
		s.fail(w, err)
		return
	}
	items, err := s.Lib.ListTrash(r.Context(), a)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": toJSONList(items), "retention_days": int(s.Cfg.Trash.Retention.D().Hours() / 24),
	})
}

func (s *Server) restoreTrash(w http.ResponseWriter, r *http.Request) {
	f, a, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	nf, err := s.Lib.Restore(r.Context(), a, f, deviceFrom(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toJSON(nf))
}

func (s *Server) purgeTrash(w http.ResponseWriter, r *http.Request) {
	f, a, ok := s.fileFromPath(w, r)
	if !ok {
		return
	}
	if !a.Write {
		s.fail(w, library.ErrForbidden)
		return
	}
	if f.State != "trashed" || f.TrashRoot != f.ID {
		s.fail(w, metadata.ErrNotFound)
		return
	}
	if err := s.Lib.PurgeTrash(r.Context(), f); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
