package metadata

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Owner identifies a user's library or a shared space.
type Owner struct {
	Kind string // "user" | "space"
	ID   int64
}

type File struct {
	ID          int64
	Owner       Owner
	Path        string
	Parent      string
	Name        string
	IsDir       bool
	Size        int64
	MTime       time.Time
	SHA256      string
	Mime        string
	Version     int64
	Seq         int64
	State       string
	TrashPath   string
	TrashRoot   int64
	TrashReason string
	TrashedAt   *time.Time
	DeviceID    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const fileCols = `id, owner_kind, owner_id, path, parent, name, is_dir, size, mtime, COALESCE(sha256, ''), mime, version, seq,
	state, COALESCE(trash_path, ''), COALESCE(trash_root, 0), COALESCE(trash_reason, ''), trashed_at, COALESCE(device_id, ''),
	created_at, updated_at`

func scanFile(row interface{ Scan(...any) error }) (*File, error) {
	var f File
	var mtime, created, updated int64
	var trashedAt sql.NullInt64
	err := row.Scan(&f.ID, &f.Owner.Kind, &f.Owner.ID, &f.Path, &f.Parent, &f.Name, &f.IsDir, &f.Size, &mtime, &f.SHA256,
		&f.Mime, &f.Version, &f.Seq, &f.State, &f.TrashPath, &f.TrashRoot, &f.TrashReason, &trashedAt, &f.DeviceID,
		&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.MTime, f.CreatedAt, f.UpdatedAt = fromMS(mtime), fromMS(created), fromMS(updated)
	if trashedAt.Valid {
		t := fromMS(trashedAt.Int64)
		f.TrashedAt = &t
	}
	return &f, nil
}

func scanFiles(rows *sql.Rows, err error) ([]*File, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ParentOf returns the parent directory of a relative path ("" for top level).
func ParentOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// prefixRange returns bounds matching every path strictly under dir:
// dir + "/" <= path < dir + "0" ('0' is the byte after '/').
func prefixRange(dir string) (lo, hi string) { return dir + "/", dir + "0" }

func (db *DB) FileByID(ctx context.Context, tx *sql.Tx, id int64) (*File, error) {
	return scanFile(db.q(tx).QueryRowContext(ctx, `SELECT `+fileCols+` FROM files WHERE id = ?`, id))
}

func (db *DB) ActiveFileByPath(ctx context.Context, tx *sql.Tx, o Owner, p string) (*File, error) {
	return scanFile(db.q(tx).QueryRowContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE owner_kind = ? AND owner_id = ? AND path = ? AND state = 'active'`, o.Kind, o.ID, p))
}

// ListChildren lists active entries directly under dir, ordered by name,
// starting after afterName.
func (db *DB) ListChildren(ctx context.Context, o Owner, dir, afterName string, limit int) ([]*File, error) {
	return scanFiles(db.sql.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE owner_kind = ? AND owner_id = ? AND parent = ? AND state = 'active' AND name > ?
		 ORDER BY name LIMIT ?`, o.Kind, o.ID, dir, afterName, limit))
}

// ActiveFiles calls fn for every active row of an owner.
func (db *DB) ActiveFiles(ctx context.Context, o Owner, fn func(*File) error) error {
	files, err := scanFiles(db.sql.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE owner_kind = ? AND owner_id = ? AND state = 'active' ORDER BY path`, o.Kind, o.ID))
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) InsertFile(ctx context.Context, tx *sql.Tx, f *File) error {
	t := now()
	f.Parent, f.Name = ParentOf(f.Path), path.Base(f.Path)
	var sha any
	if f.SHA256 != "" {
		sha = f.SHA256
	}
	var trashedAt any
	if f.TrashedAt != nil {
		trashedAt = ms(*f.TrashedAt)
	}
	res, err := db.q(tx).ExecContext(ctx,
		`INSERT INTO files (owner_kind, owner_id, path, parent, name, is_dir, size, mtime, sha256, mime, version, seq, state,
		   trash_path, trash_root, trash_reason, trashed_at, device_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?)`,
		f.Owner.Kind, f.Owner.ID, f.Path, f.Parent, f.Name, f.IsDir, f.Size, ms(f.MTime), sha, f.Mime, max(f.Version, 1), f.Seq,
		coalesce(f.State, "active"), f.TrashPath, f.TrashRoot, f.TrashReason, trashedAt, f.DeviceID, t, t)
	if err != nil {
		if isUnique(err) {
			return ErrDuplicate
		}
		return err
	}
	f.ID, _ = res.LastInsertId()
	f.CreatedAt, f.UpdatedAt = fromMS(t), fromMS(t)
	if f.Version == 0 {
		f.Version = 1
	}
	if f.State == "" {
		f.State = "active"
	}
	return nil
}

// UpdateContent stores a new version of an existing file row.
func (db *DB) UpdateContent(ctx context.Context, tx *sql.Tx, f *File) error {
	t := now()
	_, err := db.q(tx).ExecContext(ctx,
		`UPDATE files SET size = ?, mtime = ?, sha256 = ?, mime = ?, version = ?, device_id = NULLIF(?, ''), updated_at = ? WHERE id = ?`,
		f.Size, ms(f.MTime), f.SHA256, f.Mime, f.Version, f.DeviceID, t, f.ID)
	f.UpdatedAt = fromMS(t)
	return err
}

// AddChange records a change and stamps the file row with its sequence number.
func (db *DB) AddChange(ctx context.Context, tx *sql.Tx, f *File, op, oldPath string) (int64, error) {
	var seq int64
	err := db.q(tx).QueryRowContext(ctx,
		`INSERT INTO changes (owner_kind, owner_id, file_id, op, path, old_path, at) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?) RETURNING seq`,
		f.Owner.Kind, f.Owner.ID, f.ID, op, f.Path, oldPath, now()).Scan(&seq)
	if err != nil {
		return 0, err
	}
	if _, err := db.q(tx).ExecContext(ctx, `UPDATE files SET seq = ? WHERE id = ?`, seq, f.ID); err != nil {
		return 0, err
	}
	f.Seq = seq
	return seq, nil
}

// MoveTree renames an active entry and, for directories, all descendants.
func (db *DB) MoveTree(ctx context.Context, tx *sql.Tx, o Owner, oldPath, newPath string) error {
	q := db.q(tx)
	t := now()
	if _, err := q.ExecContext(ctx,
		`UPDATE files SET path = ?, parent = ?, name = ?, updated_at = ? WHERE owner_kind = ? AND owner_id = ? AND path = ? AND state = 'active'`,
		newPath, ParentOf(newPath), path.Base(newPath), t, o.Kind, o.ID, oldPath); err != nil {
		return err
	}
	lo, hi := prefixRange(oldPath)
	// SQLite substr() counts characters, so offsets must be rune counts.
	cut := utf8.RuneCountInString(oldPath) + 1
	_, err := q.ExecContext(ctx,
		`UPDATE files SET path = ? || substr(path, ?), parent = ? || substr(parent, ?), updated_at = ?
		 WHERE owner_kind = ? AND owner_id = ? AND state = 'active' AND path >= ? AND path < ?`,
		newPath, cut, newPath, cut, t, o.Kind, o.ID, lo, hi)
	return err
}

// HasActiveDescendants reports whether dir has any active entries beneath it.
func (db *DB) HasActiveDescendants(ctx context.Context, tx *sql.Tx, o Owner, dir string) (bool, error) {
	lo, hi := prefixRange(dir)
	var n int
	err := db.q(tx).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE owner_kind = ? AND owner_id = ? AND state = 'active' AND path >= ? AND path < ?`,
		o.Kind, o.ID, lo, hi).Scan(&n)
	return n > 0, err
}

// TrashTree marks root (and its descendants, for directories) as trashed.
func (db *DB) TrashTree(ctx context.Context, tx *sql.Tx, root *File, trashPath, reason string) error {
	q := db.q(tx)
	t := now()
	if root.IsDir {
		lo, hi := prefixRange(root.Path)
		if _, err := q.ExecContext(ctx,
			`UPDATE files SET state = 'trashed', trash_root = ?, trashed_at = ?, updated_at = ?
			 WHERE owner_kind = ? AND owner_id = ? AND state = 'active' AND path >= ? AND path < ?`,
			root.ID, t, t, root.Owner.Kind, root.Owner.ID, lo, hi); err != nil {
			return err
		}
	}
	_, err := q.ExecContext(ctx,
		`UPDATE files SET state = 'trashed', trash_path = ?, trash_root = id, trash_reason = ?, trashed_at = ?, updated_at = ? WHERE id = ?`,
		trashPath, reason, t, t, root.ID)
	if err == nil {
		root.State, root.TrashPath, root.TrashRoot, root.TrashReason = "trashed", trashPath, root.ID, reason
		tt := fromMS(t)
		root.TrashedAt = &tt
	}
	return err
}

// RestoreTree reactivates a trashed root and its descendants at newPath.
func (db *DB) RestoreTree(ctx context.Context, tx *sql.Tx, root *File, newPath string) error {
	q := db.q(tx)
	t := now()
	cut := utf8.RuneCountInString(root.Path) + 1
	if _, err := q.ExecContext(ctx,
		`UPDATE files SET state = 'active', path = ? || substr(path, ?), parent = ? || substr(parent, ?),
		   trash_root = NULL, trashed_at = NULL, updated_at = ?
		 WHERE trash_root = ? AND id != ? AND state = 'trashed'`,
		newPath, cut, newPath, cut, t, root.ID, root.ID); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx,
		`UPDATE files SET state = 'active', path = ?, parent = ?, name = ?, trash_path = NULL, trash_root = NULL,
		   trash_reason = NULL, trashed_at = NULL, updated_at = ? WHERE id = ?`,
		newPath, ParentOf(newPath), path.Base(newPath), t, root.ID)
	return err
}

// TrashedRoots lists trash entries for an owner, newest first.
func (db *DB) TrashedRoots(ctx context.Context, o Owner) ([]*File, error) {
	return scanFiles(db.sql.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE owner_kind = ? AND owner_id = ? AND state = 'trashed' AND trash_root = id
		 ORDER BY trashed_at DESC`, o.Kind, o.ID))
}

// ExpiredTrashRoots lists trash roots trashed before the cutoff.
func (db *DB) ExpiredTrashRoots(ctx context.Context, before time.Time) ([]*File, error) {
	return scanFiles(db.sql.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE state = 'trashed' AND trash_root = id AND trashed_at < ?`, ms(before)))
}

// DeleteTrashTree removes the rows of a purged trash entry.
func (db *DB) DeleteTrashTree(ctx context.Context, tx *sql.Tx, rootID int64) error {
	_, err := db.q(tx).ExecContext(ctx, `DELETE FROM files WHERE trash_root = ? AND state = 'trashed'`, rootID)
	return err
}

// DeleteFile removes a row outright (used by reindex for files gone from disk).
func (db *DB) DeleteFile(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := db.q(tx).ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	return err
}

// ---- Content lookups (dedup) -------------------------------------------------

// AnyActiveByContent returns active files with the given content, any owner.
func (db *DB) AnyActiveByContent(ctx context.Context, tx *sql.Tx, sha string, size int64, limit int) ([]*File, error) {
	return scanFiles(db.q(tx).QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE sha256 = ? AND size = ? AND state = 'active' AND is_dir = 0 LIMIT ?`, sha, size, limit))
}

// AnyActiveBySHA returns active files with the given content hash.
func (db *DB) AnyActiveBySHA(ctx context.Context, sha string, limit int) ([]*File, error) {
	return scanFiles(db.sql.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE sha256 = ? AND state = 'active' AND is_dir = 0 LIMIT ?`, sha, limit))
}

// VisibleOwners returns the owners a user can read: their library and their spaces.
func (db *DB) VisibleOwners(ctx context.Context, userID int64) ([]Owner, error) {
	out := []Owner{{Kind: "user", ID: userID}}
	rows, err := db.sql.QueryContext(ctx, `SELECT space_id FROM space_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, Owner{Kind: "space", ID: id})
	}
	return out, rows.Err()
}

func ownerFilter(owners []Owner) (string, []any) {
	var parts []string
	var args []any
	for _, o := range owners {
		parts = append(parts, `(owner_kind = ? AND owner_id = ?)`)
		args = append(args, o.Kind, o.ID)
	}
	if len(parts) == 0 {
		return "0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// HaveContent returns which of the given hashes exist as active files, limited
// to owners if non-nil.
func (db *DB) HaveContent(ctx context.Context, hashes []string, owners []Owner) (map[string]bool, error) {
	out := make(map[string]bool)
	for start := 0; start < len(hashes); start += 500 {
		batch := hashes[start:min(start+500, len(hashes))]
		args := make([]any, 0, len(batch)+2*len(owners))
		for _, h := range batch {
			args = append(args, h)
		}
		q := `SELECT DISTINCT sha256 FROM files WHERE state = 'active' AND sha256 IN (?` + strings.Repeat(",?", len(batch)-1) + `)`
		if owners != nil {
			f, fargs := ownerFilter(owners)
			q += " AND " + f
			args = append(args, fargs...)
		}
		rows, err := db.sql.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return nil, err
			}
			out[h] = true
		}
		rows.Close()
	}
	return out, nil
}

// ---- Change feed -------------------------------------------------------------

type Change struct {
	Seq     int64
	Owner   Owner
	FileID  int64
	Op      string
	Path    string
	OldPath string
	At      time.Time
}

func (db *DB) Changes(ctx context.Context, owners []Owner, after int64, limit int) ([]Change, error) {
	f, args := ownerFilter(owners)
	args = append([]any{after}, append(args, limit)...)
	rows, err := db.sql.QueryContext(ctx,
		`SELECT seq, owner_kind, owner_id, file_id, op, path, COALESCE(old_path, ''), at FROM changes
		 WHERE seq > ? AND `+f+` ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var at int64
		if err := rows.Scan(&c.Seq, &c.Owner.Kind, &c.Owner.ID, &c.FileID, &c.Op, &c.Path, &c.OldPath, &at); err != nil {
			return nil, err
		}
		c.At = fromMS(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) MaxSeq(ctx context.Context) (int64, error) {
	var n int64
	err := db.sql.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM changes`).Scan(&n)
	return n, err
}

func coalesce(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
