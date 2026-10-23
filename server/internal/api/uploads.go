package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/library"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

type startUploadReq struct {
	SpaceID     int64     `json:"space_id"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	MTime       time.Time `json:"mtime"`
	BaseVersion *int64    `json:"base_version"`
}

type uploadResp struct {
	Status       string     `json:"status"` // "pending" | "completed"
	UploadID     string     `json:"upload_id,omitempty"`
	PartSize     int64      `json:"part_size,omitempty"`
	Parts        int        `json:"parts,omitempty"`
	Received     []int      `json:"received,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	File         *FileJSON  `json:"file,omitempty"`
	Deduplicated bool       `json:"deduplicated,omitempty"`
	Conflict     bool       `json:"conflict,omitempty"`
	Unchanged    bool       `json:"unchanged,omitempty"`
}

func completed(res *library.CommitResult, dedup bool) uploadResp {
	j := toJSON(res.File)
	return uploadResp{Status: "completed", File: &j, Deduplicated: dedup, Conflict: res.Conflict, Unchanged: res.Unchanged}
}

// startUpload begins an upload. If the server already holds the content (and
// the dedup scope allows telling this user), the file is linked in immediately
// and no bytes need to be sent.
func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) {
	var req startUploadReq
	if !readJSON(w, r, &req) {
		return
	}
	if !sha256Re.MatchString(req.SHA256) {
		writeErr(w, http.StatusBadRequest, "bad_request", "sha256 must be 64 lowercase hex characters")
		return
	}
	if req.Size < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "size must be >= 0")
		return
	}
	if req.MTime.IsZero() {
		req.MTime = time.Now()
	}
	a, err := s.access(r, req.SpaceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !a.Write {
		s.fail(w, library.ErrForbidden)
		return
	}
	if err := s.Lib.Guard().Writable(); err != nil {
		s.fail(w, err)
		return
	}
	p, err := storage.CleanPath(req.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	if p == "" {
		s.fail(w, fmt.Errorf("%w: empty path", storage.ErrInvalidPath))
		return
	}
	u, d := userFrom(r), deviceFrom(r)
	commit := library.CommitRequest{
		Access: a, Path: p, SHA256: req.SHA256, Size: req.Size, MTime: req.MTime,
		BaseVersion: req.BaseVersion, DeviceID: d.ID, DeviceName: d.Name,
	}

	if have, err := s.Lib.FindForUpload(r.Context(), u, req.SHA256, req.Size); err != nil {
		s.fail(w, err)
		return
	} else if have {
		res, err := s.Lib.LinkExisting(r.Context(), commit)
		if err == nil {
			writeJSON(w, http.StatusOK, completed(res, !res.Unchanged))
			return
		}
		if !errors.Is(err, metadata.ErrNotFound) {
			s.fail(w, err)
			return
		}
		// Index says we have it but no readable copy exists: take the upload.
	}

	sess := &metadata.UploadSession{
		ID: uuid.NewString(), UserID: u.ID, DeviceID: d.ID, Owner: a.Owner, Path: p,
		Size: req.Size, SHA256: req.SHA256, MTime: req.MTime, BaseVersion: req.BaseVersion,
		PartSize: s.Cfg.Uploads.PartSize, ExpiresAt: time.Now().Add(s.Cfg.Uploads.Expiry.D()),
	}
	if err := os.MkdirAll(s.stagingDir(sess.ID), 0o700); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.DB.CreateUpload(r.Context(), sess); err != nil {
		s.fail(w, err)
		return
	}
	exp := sess.ExpiresAt.UTC()
	writeJSON(w, http.StatusCreated, uploadResp{
		Status: "pending", UploadID: sess.ID, PartSize: sess.PartSize, Parts: sess.Parts(), Received: []int{}, ExpiresAt: &exp,
	})
}

func (s *Server) stagingDir(id string) string { return s.Cfg.StatePath("staging", id) }

func (s *Server) partPath(id string, n int) string {
	return filepath.Join(s.stagingDir(id), fmt.Sprintf("%06d.part", n))
}

func (s *Server) loadUpload(w http.ResponseWriter, r *http.Request) (*metadata.UploadSession, bool) {
	sess, err := s.DB.Upload(r.Context(), r.PathValue("id"))
	if err != nil || sess.UserID != userFrom(r).ID {
		s.fail(w, metadata.ErrNotFound)
		return nil, false
	}
	if time.Now().After(sess.ExpiresAt) {
		s.removeUpload(r, sess.ID)
		writeErr(w, http.StatusGone, "upload_expired", "upload session expired; start again")
		return nil, false
	}
	return sess, true
}

func (s *Server) receivedParts(sess *metadata.UploadSession) []int {
	out := []int{}
	for n := 0; n < sess.Parts(); n++ {
		if _, err := os.Stat(s.partPath(sess.ID, n)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) uploadStatus(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	exp := sess.ExpiresAt.UTC()
	writeJSON(w, http.StatusOK, uploadResp{
		Status: "pending", UploadID: sess.ID, PartSize: sess.PartSize, Parts: sess.Parts(),
		Received: s.receivedParts(sess), ExpiresAt: &exp,
	})
}

// uploadPart stores one part. Parts are idempotent: re-sending a part
// replaces it. An optional X-Part-SHA256 header is verified.
func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= sess.Parts() {
		writeErr(w, http.StatusBadRequest, "bad_part", fmt.Sprintf("part must be in [0, %d)", sess.Parts()))
		return
	}
	want := sess.PartSize
	if n == sess.Parts()-1 {
		want = sess.Size - int64(n)*sess.PartSize
	}
	tmp, err := os.CreateTemp(s.stagingDir(sess.ID), "recv-*")
	if err != nil {
		s.fail(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	got, err := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, want+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_part", "reading part: "+err.Error())
		return
	}
	if got != want {
		writeErr(w, http.StatusBadRequest, "bad_part", fmt.Sprintf("part %d must be %d bytes, got %d", n, want, got))
		return
	}
	if ps := r.Header.Get("X-Part-SHA256"); ps != "" && ps != hex.EncodeToString(h.Sum(nil)) {
		writeErr(w, http.StatusUnprocessableEntity, "hash_mismatch", "part hash mismatch")
		return
	}
	if err := os.Rename(tmp.Name(), s.partPath(sess.ID, n)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// completeUpload assembles the parts into the library temp dir, verifies the
// whole-file hash, and commits.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	if got := s.receivedParts(sess); len(got) != sess.Parts() {
		writeErr(w, http.StatusConflict, "incomplete", fmt.Sprintf("received %d of %d parts", len(got), sess.Parts()))
		return
	}
	readers := make([]io.Reader, 0, sess.Parts())
	for n := 0; n < sess.Parts(); n++ {
		f, err := os.Open(s.partPath(sess.ID, n))
		if err != nil {
			s.fail(w, err)
			return
		}
		defer f.Close()
		readers = append(readers, f)
	}
	tmp, size, sum, err := s.Lib.CopyToTemp(io.MultiReader(readers...))
	if err != nil {
		s.fail(w, err)
		return
	}
	if size != sess.Size || sum != sess.SHA256 {
		os.Remove(tmp)
		s.removeUpload(r, sess.ID)
		s.fail(w, fmt.Errorf("%w: got %d bytes with sha256 %s", library.ErrHashMismatch, size, sum))
		return
	}
	a, err := s.Lib.AccessForOwner(r.Context(), userFrom(r), sess.Owner)
	if err != nil {
		os.Remove(tmp)
		s.fail(w, err)
		return
	}
	d := deviceFrom(r)
	res, err := s.Lib.Commit(r.Context(), library.CommitRequest{
		Access: a, Path: sess.Path, TempPath: tmp, SHA256: sum, Size: size, MTime: sess.MTime,
		BaseVersion: sess.BaseVersion, DeviceID: d.ID, DeviceName: d.Name,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.removeUpload(r, sess.ID)
	writeJSON(w, http.StatusOK, completed(res, res.Linked != "" && res.Linked != storage.LinkOff))
}

func (s *Server) abortUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	s.removeUpload(r, sess.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeUpload(r *http.Request, id string) {
	os.RemoveAll(s.stagingDir(id))
	s.DB.DeleteUpload(r.Context(), id)
}
