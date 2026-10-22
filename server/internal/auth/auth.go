// Package auth handles password hashing, access/refresh tokens and device pairing.
//
// Access tokens are short-lived, stateless and HMAC-signed. Refresh tokens are
// random, bound to a device, stored hashed, and rotated on every use.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrWeakPassword       = errors.New("password too short")
)

// ---- Passwords ---------------------------------------------------------------

type argonParams struct {
	time, memory uint32
	threads      uint8
}

var defaultArgon = argonParams{time: 3, memory: 64 * 1024, threads: 2}

// HashPassword returns a PHC-formatted Argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := defaultArgon
	key := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a hash produced by HashPassword.
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when the user doesn't exist, so login timing
// doesn't reveal which usernames are valid.
var dummyHash, _ = HashPassword("not-a-real-password")

// ---- Service -------------------------------------------------------------------

type Config struct {
	AccessTTL, RefreshTTL, PairingTTL time.Duration
	MinPasswordChars                  int
}

type Service struct {
	db     *metadata.DB
	cfg    Config
	secret []byte
}

// NewService loads (or creates) the token signing key in secretsDir.
func NewService(db *metadata.DB, cfg Config, secretsDir string) (*Service, error) {
	key, err := loadOrCreateKey(filepath.Join(secretsDir, "token.key"))
	if err != nil {
		return nil, err
	}
	return &Service{db: db, cfg: cfg, secret: key}, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil && len(data) == 32 {
		return data, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, key, 0o600)
}

func (s *Service) CheckPasswordPolicy(pw string) error {
	if len([]rune(pw)) < s.cfg.MinPasswordChars {
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, s.cfg.MinPasswordChars)
	}
	return nil
}

// CreateUser validates the password and stores a new user.
func (s *Service) CreateUser(ctx context.Context, username, displayName, password, role string) (*metadata.User, error) {
	if err := s.CheckPasswordPolicy(password); err != nil {
		return nil, err
	}
	if role != "admin" && role != "member" {
		return nil, fmt.Errorf("role must be admin or member")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &metadata.User{Username: username, DisplayName: displayName, PasswordHash: hash, Role: role}
	if err := s.db.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Service) SetPassword(ctx context.Context, userID int64, password string) error {
	if err := s.CheckPasswordPolicy(password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.db.SetPassword(ctx, userID, hash)
}

// Tokens is what clients receive after login, pairing or refresh.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	AccessExp    time.Time `json:"access_expires_at"`
	RefreshToken string    `json:"refresh_token"`
	RefreshExp   time.Time `json:"refresh_expires_at"`
	DeviceID     string    `json:"device_id"`
	UserID       int64     `json:"user_id"`
}

// Login checks credentials and registers a new device.
func (s *Service) Login(ctx context.Context, username, password, deviceName, platform string) (*Tokens, error) {
	u, err := s.db.UserByName(ctx, username)
	if err != nil {
		VerifyPassword(dummyHash, password)
		if errors.Is(err, metadata.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if !VerifyPassword(u.PasswordHash, password) || u.Disabled {
		return nil, ErrInvalidCredentials
	}
	return s.newDevice(ctx, u, deviceName, platform)
}

func (s *Service) newDevice(ctx context.Context, u *metadata.User, name, platform string) (*Tokens, error) {
	if name == "" {
		name = "unnamed device"
	}
	d := &metadata.Device{ID: uuid.NewString(), UserID: u.ID, Name: name, Platform: platform}
	secret, hash := newRefreshSecret()
	exp := time.Now().Add(s.cfg.RefreshTTL)
	if err := s.db.CreateDevice(ctx, d, hash, exp); err != nil {
		return nil, err
	}
	return s.issue(u, d.ID, secret, exp)
}

// Refresh exchanges a refresh token for new tokens (rotating the refresh token).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	deviceID, secret, ok := strings.Cut(refreshToken, ".")
	if !ok {
		return nil, ErrInvalidToken
	}
	d, err := s.db.DeviceByID(ctx, deviceID)
	if err != nil {
		return nil, ErrInvalidToken
	}
	oldHash := hashSecret(secret)
	if d.RevokedAt != nil || time.Now().After(d.RefreshExpiry()) ||
		subtle.ConstantTimeCompare([]byte(oldHash), []byte(d.RefreshHash())) != 1 {
		return nil, ErrInvalidToken
	}
	u, err := s.db.UserByID(ctx, d.UserID)
	if err != nil || u.Disabled {
		return nil, ErrInvalidToken
	}
	newSecret, newHash := newRefreshSecret()
	exp := time.Now().Add(s.cfg.RefreshTTL)
	ok, err = s.db.RotateRefresh(ctx, d.ID, oldHash, newHash, exp)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidToken
	}
	return s.issue(u, d.ID, newSecret, exp)
}

// CreatePairingToken returns a one-time token that a new device can exchange
// for credentials (shown to the user as a QR code).
func (s *Service) CreatePairingToken(ctx context.Context, userID int64) (string, time.Time, error) {
	tok := randomString(24)
	exp := time.Now().Add(s.cfg.PairingTTL)
	return tok, exp, s.db.CreatePairingToken(ctx, userID, hashSecret(tok), exp)
}

// Pair consumes a pairing token and registers the device.
func (s *Service) Pair(ctx context.Context, token, deviceName, platform string) (*Tokens, error) {
	userID, err := s.db.ConsumePairingToken(ctx, hashSecret(token))
	if err != nil {
		return nil, ErrInvalidToken
	}
	u, err := s.db.UserByID(ctx, userID)
	if err != nil || u.Disabled {
		return nil, ErrInvalidToken
	}
	return s.newDevice(ctx, u, deviceName, platform)
}

// ---- Access tokens -------------------------------------------------------------

type Claims struct {
	UserID   int64  `json:"uid"`
	DeviceID string `json:"did"`
	Role     string `json:"role"`
	Exp      int64  `json:"exp"`
}

func (s *Service) issue(u *metadata.User, deviceID, refreshSecret string, refreshExp time.Time) (*Tokens, error) {
	exp := time.Now().Add(s.cfg.AccessTTL)
	at, err := s.sign(Claims{UserID: u.ID, DeviceID: deviceID, Role: u.Role, Exp: exp.Unix()})
	if err != nil {
		return nil, err
	}
	return &Tokens{
		AccessToken: at, AccessExp: exp.UTC(),
		RefreshToken: deviceID + "." + refreshSecret, RefreshExp: refreshExp.UTC(),
		DeviceID: deviceID, UserID: u.ID,
	}, nil
}

func (s *Service) sign(c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := "v1." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify checks an access token's signature and expiry. Callers must still
// check the user and device are active.
func (s *Service) Verify(token string) (*Claims, error) {
	i := strings.LastIndexByte(token, '.')
	if i < 0 || !strings.HasPrefix(token, "v1.") {
		return nil, ErrInvalidToken
	}
	body, sig := token[:i], token[i+1:]
	gotSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return nil, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(body, "v1."))
	if err != nil {
		return nil, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrInvalidToken
	}
	if time.Now().Unix() >= c.Exp {
		return nil, ErrInvalidToken
	}
	return &c, nil
}

func newRefreshSecret() (secret, hash string) {
	secret = randomString(32)
	return secret, hashSecret(secret)
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomCode returns a short human-typeable code (used for the first-run setup code).
func RandomCode() string {
	b := make([]byte, 5)
	rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}
