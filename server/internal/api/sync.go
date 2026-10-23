package api

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
)

func (s *Server) have(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SHA256 []string `json:"sha256"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.SHA256) > 5000 {
		writeErr(w, http.StatusBadRequest, "bad_request", "at most 5000 hashes per request")
		return
	}
	for _, h := range req.SHA256 {
		if !sha256Re.MatchString(h) {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid sha256: "+h)
			return
		}
	}
	have, err := s.Lib.Have(r.Context(), userFrom(r), req.SHA256)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []string{}
	for _, h := range req.SHA256 {
		if have[h] {
			out = append(out, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"have": out})
}

type changeJSON struct {
	Seq     int64     `json:"seq"`
	Op      string    `json:"op"`
	FileID  int64     `json:"file_id"`
	SpaceID *int64    `json:"space_id,omitempty"`
	Path    string    `json:"path"`
	OldPath string    `json:"old_path,omitempty"`
	At      time.Time `json:"at"`
	File    *FileJSON `json:"file,omitempty"` // current state; absent if purged
}

// changes returns the change feed after a cursor. With wait > 0 it long-polls
// until something changes or the wait expires.
//
// Clients apply a directory "move" or "trash" to everything under old_path.
func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	cursor := queryInt(r, "cursor", 0)
	limit := int(min(max(queryInt(r, "limit", 1000), 1), 5000))
	wait := time.Duration(min(max(queryInt(r, "wait", 0), 0), 60)) * time.Second

	owners, err := s.DB.VisibleOwners(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	var list []metadata.Change
	for {
		list, err = s.DB.Changes(r.Context(), owners, cursor, limit+1)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(list) > 0 || wait == 0 || ctx.Err() != nil {
			break
		}
		// Wake on any change (or every few seconds, to catch CLI writes).
		waitCtx, c := context.WithTimeout(ctx, 5*time.Second)
		s.Notifier.Wait(waitCtx, max(cursor, s.Notifier.Seq()))
		c()
	}
	hasMore := len(list) > limit
	if hasMore {
		list = list[:limit]
	}
	out := make([]changeJSON, 0, len(list))
	next := cursor
	for _, c := range list {
		cj := changeJSON{Seq: c.Seq, Op: c.Op, FileID: c.FileID, Path: c.Path, OldPath: c.OldPath, At: c.At}
		if c.Owner.Kind == "space" {
			id := c.Owner.ID
			cj.SpaceID = &id
		}
		if f, err := s.DB.FileByID(r.Context(), nil, c.FileID); err == nil {
			j := toJSON(f)
			cj.File = &j
		}
		out = append(out, cj)
		next = c.Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out, "cursor": next, "has_more": hasMore})
}

// cursor returns the latest sequence, so a client that just did a full
// listing can follow changes from now on.
func (s *Server) cursor(w http.ResponseWriter, r *http.Request) {
	seq, err := s.DB.MaxSeq(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cursor": seq})
}

// events pushes {"type":"changes","seq":N} whenever anything changes, so
// clients know to call /sync/changes. It carries no file data.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())
	seq := s.Notifier.Seq()
	if err := wsjson.Write(ctx, c, map[string]any{"type": "hello", "seq": seq}); err != nil {
		return
	}
	for {
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		next := s.Notifier.Wait(waitCtx, seq)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if next > seq {
			seq = next
			if err := wsjson.Write(ctx, c, map[string]any{"type": "changes", "seq": seq}); err != nil {
				return
			}
			continue
		}
		pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.Ping(pctx)
		pcancel()
		if err != nil {
			return
		}
	}
}
