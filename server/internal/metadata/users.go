package metadata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	DisplayName  string    `json:"display_name"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	QuotaBytes   int64     `json:"quota_bytes"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

var ErrDuplicate = errors.New("already exists")

const userCols = `id, username, display_name, password_hash, role, quota_bytes, disabled, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created int64
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.QuotaBytes, &u.Disabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = fromMS(created)
	return &u, nil
}

func (db *DB) CreateUser(ctx context.Context, u *User) error {
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO users (username, display_name, password_hash, role, quota_bytes, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		u.Username, u.DisplayName, u.PasswordHash, u.Role, u.QuotaBytes, now())
	if err != nil {
		if isUnique(err) {
			return ErrDuplicate
		}
		return err
	}
	u.ID, _ = res.LastInsertId()
	return nil
}

func (db *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(db.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (db *DB) UserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(db.sql.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

func (db *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UpdateUser saves display name, role, quota and disabled flag.
func (db *DB) UpdateUser(ctx context.Context, u *User) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE users SET display_name = ?, role = ?, quota_bytes = ?, disabled = ? WHERE id = ?`,
		u.DisplayName, u.Role, u.QuotaBytes, u.Disabled, u.ID)
	return err
}

func (db *DB) SetPassword(ctx context.Context, userID int64, hash string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

// UserUsage is the logical bytes a user owns (dedup does not reduce it).
func (db *DB) UserUsage(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size), 0) FROM files WHERE owner_kind = 'user' AND owner_id = ? AND state = 'active'`, userID).Scan(&n)
	return n, err
}

// ---- Devices ----------------------------------------------------------------

type Device struct {
	ID         string     `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`

	refreshHash string
	refreshExp  time.Time
}

const deviceCols = `id, user_id, name, platform, refresh_hash, refresh_exp, created_at, last_seen_at, revoked_at`

func scanDevice(row interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var exp, created, seen int64
	var revoked sql.NullInt64
	err := row.Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.refreshHash, &exp, &created, &seen, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.refreshExp, d.CreatedAt, d.LastSeenAt = fromMS(exp), fromMS(created), fromMS(seen)
	if revoked.Valid {
		t := fromMS(revoked.Int64)
		d.RevokedAt = &t
	}
	return &d, nil
}

func (d *Device) RefreshHash() string      { return d.refreshHash }
func (d *Device) RefreshExpiry() time.Time { return d.refreshExp }

func (db *DB) CreateDevice(ctx context.Context, d *Device, refreshHash string, refreshExp time.Time) error {
	t := now()
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO devices (id, user_id, name, platform, refresh_hash, refresh_exp, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.UserID, d.Name, d.Platform, refreshHash, ms(refreshExp), t, t)
	d.CreatedAt, d.LastSeenAt = fromMS(t), fromMS(t)
	return err
}

func (db *DB) DeviceByID(ctx context.Context, id string) (*Device, error) {
	return scanDevice(db.sql.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

func (db *DB) ListDevices(ctx context.Context, userID int64) ([]*Device, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RotateRefresh replaces a device's refresh token only if the old hash still
// matches, so a stolen-and-replayed refresh token loses the race.
func (db *DB) RotateRefresh(ctx context.Context, deviceID, oldHash, newHash string, exp time.Time) (bool, error) {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE devices SET refresh_hash = ?, refresh_exp = ?, last_seen_at = ? WHERE id = ? AND refresh_hash = ? AND revoked_at IS NULL`,
		newHash, ms(exp), now(), deviceID, oldHash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (db *DB) RevokeDevice(ctx context.Context, userID int64, deviceID string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE devices SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, now(), deviceID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- Pairing tokens ---------------------------------------------------------

func (db *DB) CreatePairingToken(ctx context.Context, userID int64, tokenHash string, exp time.Time) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO pairing_tokens (token_hash, user_id, expires_at) VALUES (?, ?, ?)`, tokenHash, userID, ms(exp))
	return err
}

// ConsumePairingToken marks a token used and returns its user, atomically.
func (db *DB) ConsumePairingToken(ctx context.Context, tokenHash string) (int64, error) {
	var userID int64
	err := db.sql.QueryRowContext(ctx,
		`UPDATE pairing_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? RETURNING user_id`,
		now(), tokenHash, now()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}

// ---- Spaces -----------------------------------------------------------------

type Space struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	Permission string    `json:"permission,omitempty"` // caller's permission when listed for a user
}

type SpaceMember struct {
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	Permission string `json:"permission"`
}

func (db *DB) CreateSpace(ctx context.Context, name string, createdBy int64) (*Space, error) {
	t := now()
	res, err := db.sql.ExecContext(ctx, `INSERT INTO spaces (name, created_by, created_at) VALUES (?, NULLIF(?, 0), ?)`, name, createdBy, t)
	if err != nil {
		if isUnique(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Space{ID: id, Name: name, CreatedAt: fromMS(t)}, nil
}

func (db *DB) SpaceByID(ctx context.Context, id int64) (*Space, error) {
	var s Space
	var created int64
	err := db.sql.QueryRowContext(ctx, `SELECT id, name, created_at FROM spaces WHERE id = ?`, id).Scan(&s.ID, &s.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	s.CreatedAt = fromMS(created)
	return &s, err
}

func (db *DB) SpaceByName(ctx context.Context, name string) (*Space, error) {
	var id int64
	err := db.sql.QueryRowContext(ctx, `SELECT id FROM spaces WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return db.SpaceByID(ctx, id)
}

// ListSpaces returns all spaces (userID = 0) or the spaces a user belongs to.
func (db *DB) ListSpaces(ctx context.Context, userID int64) ([]*Space, error) {
	var rows *sql.Rows
	var err error
	if userID == 0 {
		rows, err = db.sql.QueryContext(ctx, `SELECT id, name, created_at, '' FROM spaces ORDER BY name`)
	} else {
		rows, err = db.sql.QueryContext(ctx,
			`SELECT s.id, s.name, s.created_at, m.permission FROM spaces s JOIN space_members m ON m.space_id = s.id
			 WHERE m.user_id = ? ORDER BY s.name`, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Space
	for rows.Next() {
		var s Space
		var created int64
		if err := rows.Scan(&s.ID, &s.Name, &created, &s.Permission); err != nil {
			return nil, err
		}
		s.CreatedAt = fromMS(created)
		out = append(out, &s)
	}
	return out, rows.Err()
}

func (db *DB) SetSpaceMember(ctx context.Context, spaceID, userID int64, permission string) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO space_members (space_id, user_id, permission) VALUES (?, ?, ?)
		 ON CONFLICT(space_id, user_id) DO UPDATE SET permission = excluded.permission`, spaceID, userID, permission)
	return err
}

func (db *DB) RemoveSpaceMember(ctx context.Context, spaceID, userID int64) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM space_members WHERE space_id = ? AND user_id = ?`, spaceID, userID)
	return err
}

func (db *DB) SpaceMembers(ctx context.Context, spaceID int64) ([]SpaceMember, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT u.id, u.username, m.permission FROM space_members m JOIN users u ON u.id = m.user_id
		 WHERE m.space_id = ? ORDER BY u.username`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpaceMember
	for rows.Next() {
		var m SpaceMember
		if err := rows.Scan(&m.UserID, &m.Username, &m.Permission); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SpacePermission returns the user's permission on a space ("" if none).
func (db *DB) SpacePermission(ctx context.Context, spaceID, userID int64) (string, error) {
	var p string
	err := db.sql.QueryRowContext(ctx,
		`SELECT permission FROM space_members WHERE space_id = ? AND user_id = ?`, spaceID, userID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return p, err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
