package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
)

func newService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := metadata.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := NewService(db, Config{AccessTTL: time.Minute, RefreshTTL: time.Hour, PairingTTL: time.Minute, MinPasswordChars: 10}, dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "wrong") || VerifyPassword("garbage", "x") {
		t.Fatal("password verification broken")
	}
}

func TestLoginRefreshRotation(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "alice", "", "short", "member"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("expected weak password error, got %v", err)
	}
	if _, err := s.CreateUser(ctx, "alice", "Alice", "a long enough password", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "alice", "nope nope nope", "phone", "ios"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("wrong password accepted")
	}
	if _, err := s.Login(ctx, "bob", "whatever whatever", "phone", "ios"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("unknown user accepted")
	}
	tok, err := s.Login(ctx, "alice", "a long enough password", "phone", "ios")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(tok.AccessToken)
	if err != nil || c.DeviceID != tok.DeviceID {
		t.Fatalf("access token invalid: %v", err)
	}
	if _, err := s.Verify(tok.AccessToken + "x"); err == nil {
		t.Fatal("tampered token accepted")
	}
	tok2, err := s.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tok.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("a used refresh token must not work again")
	}
	if _, err := s.Refresh(ctx, tok2.RefreshToken); err != nil {
		t.Fatal(err)
	}
}

func TestPairingTokenSingleUse(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "bob", "", "a long enough password", "member")
	if err != nil {
		t.Fatal(err)
	}
	pt, _, err := s.CreatePairingToken(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Pair(ctx, pt, "Bob's phone", "android")
	if err != nil || tok.UserID != u.ID {
		t.Fatalf("pairing failed: %v", err)
	}
	if _, err := s.Pair(ctx, pt, "again", "android"); err == nil {
		t.Fatal("pairing token reused")
	}
}

func TestExpiredAccessToken(t *testing.T) {
	s := newService(t)
	tok, _ := s.sign(Claims{UserID: 1, DeviceID: "d", Exp: time.Now().Add(-time.Second).Unix()})
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("expired token accepted")
	}
}
