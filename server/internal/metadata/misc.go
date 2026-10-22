package metadata

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SetOwnTrashRoot makes a freshly inserted trashed row the root of its own
// trash entry (used for replaced versions).
func (db *DB) SetOwnTrashRoot(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := db.q(tx).ExecContext(ctx, `UPDATE files SET trash_root = id WHERE id = ?`, id)
	return err
}

// ---- Upload sessions ----------------------------------------------------------

type UploadSession struct {
	ID          string
	UserID      int64
	DeviceID    string
	Owner       Owner
	Path        string
	Size        int64
	SHA256      string
	MTime       time.Time
	BaseVersion *int64
	PartSize    int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

func (u *UploadSession) Parts() int {
	if u.Size == 0 {
		return 0
	}
	return int((u.Size + u.PartSize - 1) / u.PartSize)
}

func (db *DB) CreateUpload(ctx context.Context, u *UploadSession) error {
	t := now()
	var base any
	if u.BaseVersion != nil {
		base = *u.BaseVersion
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO upload_sessions (id, user_id, device_id, owner_kind, owner_id, path, size, sha256, mtime, base_version,
		   part_size, created_at, expires_at) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.UserID, u.DeviceID, u.Owner.Kind, u.Owner.ID, u.Path, u.Size, u.SHA256, ms(u.MTime), base, u.PartSize, t, ms(u.ExpiresAt))
	u.CreatedAt = fromMS(t)
	return err
}

func (db *DB) Upload(ctx context.Context, id string) (*UploadSession, error) {
	var u UploadSession
	var mtime, created, expires int64
	var base sql.NullInt64
	var device sql.NullString
	err := db.sql.QueryRowContext(ctx,
		`SELECT id, user_id, device_id, owner_kind, owner_id, path, size, sha256, mtime, base_version, part_size, created_at, expires_at
		 FROM upload_sessions WHERE id = ?`, id).Scan(&u.ID, &u.UserID, &device, &u.Owner.Kind, &u.Owner.ID, &u.Path, &u.Size,
		&u.SHA256, &mtime, &base, &u.PartSize, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.DeviceID = device.String
	u.MTime, u.CreatedAt, u.ExpiresAt = fromMS(mtime), fromMS(created), fromMS(expires)
	if base.Valid {
		u.BaseVersion = &base.Int64
	}
	return &u, nil
}

func (db *DB) DeleteUpload(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM upload_sessions WHERE id = ?`, id)
	return err
}

// ExpiredUploads returns ids of sessions past their expiry.
func (db *DB) ExpiredUploads(ctx context.Context, at time.Time) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id FROM upload_sessions WHERE expires_at < ?`, ms(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---- Processor results --------------------------------------------------------

type ProcessorState struct {
	Status  string
	Version int
}

func (db *DB) ProcessorState(ctx context.Context, processor, sha string) (*ProcessorState, error) {
	var s ProcessorState
	err := db.sql.QueryRowContext(ctx,
		`SELECT status, version FROM processor_state WHERE processor = ? AND sha256 = ?`, processor, sha).Scan(&s.Status, &s.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}

func (db *DB) SetProcessorState(ctx context.Context, processor, sha string, version int, status, errMsg string) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO processor_state (processor, sha256, version, status, error, updated_at) VALUES (?, ?, ?, ?, NULLIF(?, ''), ?)
		 ON CONFLICT(processor, sha256) DO UPDATE SET version = excluded.version, status = excluded.status,
		   error = excluded.error, updated_at = excluded.updated_at`,
		processor, sha, version, status, errMsg, now())
	return err
}

type MediaInfo struct {
	SHA256      string     `json:"-"`
	TakenAt     *time.Time `json:"taken_at,omitempty"`
	Width       int        `json:"width,omitempty"`
	Height      int        `json:"height,omitempty"`
	Orientation int        `json:"orientation,omitempty"`
	CameraMake  string     `json:"camera_make,omitempty"`
	CameraModel string     `json:"camera_model,omitempty"`
	Latitude    *float64   `json:"latitude,omitempty"`
	Longitude   *float64   `json:"longitude,omitempty"`
}

func (db *DB) SaveMediaInfo(ctx context.Context, m *MediaInfo) error {
	var taken any
	if m.TakenAt != nil {
		taken = ms(*m.TakenAt)
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO media_info (sha256, taken_at, width, height, orientation, camera_make, camera_model, latitude, longitude)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sha256) DO UPDATE SET taken_at = excluded.taken_at, width = excluded.width, height = excluded.height,
		   orientation = excluded.orientation, camera_make = excluded.camera_make, camera_model = excluded.camera_model,
		   latitude = excluded.latitude, longitude = excluded.longitude`,
		m.SHA256, taken, m.Width, m.Height, m.Orientation, m.CameraMake, m.CameraModel, m.Latitude, m.Longitude)
	return err
}

func (db *DB) MediaInfo(ctx context.Context, sha string) (*MediaInfo, error) {
	var m MediaInfo
	var taken sql.NullInt64
	var lat, lon sql.NullFloat64
	var mk, model sql.NullString
	var w, h, o sql.NullInt64
	err := db.sql.QueryRowContext(ctx,
		`SELECT taken_at, width, height, orientation, camera_make, camera_model, latitude, longitude FROM media_info WHERE sha256 = ?`,
		sha).Scan(&taken, &w, &h, &o, &mk, &model, &lat, &lon)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.SHA256 = sha
	if taken.Valid {
		t := fromMS(taken.Int64)
		m.TakenAt = &t
	}
	m.Width, m.Height, m.Orientation = int(w.Int64), int(h.Int64), int(o.Int64)
	m.CameraMake, m.CameraModel = mk.String, model.String
	if lat.Valid && lon.Valid {
		m.Latitude, m.Longitude = &lat.Float64, &lon.Float64
	}
	return &m, nil
}
