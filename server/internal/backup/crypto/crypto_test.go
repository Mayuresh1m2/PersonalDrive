package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	k, _ := NewMasterKey()
	ct := k.Seal([]byte("secret photo"), []byte("aad"))
	if bytes.Contains(ct, []byte("secret")) {
		t.Fatal("plaintext visible")
	}
	pt, err := k.Open(ct, []byte("aad"))
	if err != nil || string(pt) != "secret photo" {
		t.Fatal(err)
	}
	if _, err := k.Open(ct, []byte("other aad")); err == nil {
		t.Fatal("aad not bound")
	}
	ct[len(ct)-1] ^= 1
	if _, err := k.Open(ct, []byte("aad")); err == nil {
		t.Fatal("tampering not detected")
	}
	k2, _ := NewMasterKey()
	if k.ID([]byte("x")) == k2.ID([]byte("x")) {
		t.Fatal("ids must depend on the key")
	}
}

func TestWrapUnwrap(t *testing.T) {
	KDFParams.Memory = 8 * 1024 // fast tests
	mk, _ := NewMasterKey()
	kf, err := Wrap(mk, "correct horse battery staple", LabelPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kf.Unwrap("correct horse battery staple")
	if err != nil || !got.Equal(mk) {
		t.Fatal("unwrap failed")
	}
	if _, err := kf.Unwrap("wrong"); !errors.Is(err, ErrWrongSecret) {
		t.Fatal("wrong passphrase accepted")
	}

	rk, _ := NewRecoveryKey()
	if strings.Count(rk, "-") != 12 {
		t.Fatalf("unexpected recovery key format %q", rk)
	}
	rf, _ := Wrap(mk, rk, LabelRecovery)
	sloppy := strings.ToLower(strings.ReplaceAll(rk, "-", " "))
	if got, err := rf.Unwrap(sloppy); err != nil || !got.Equal(mk) {
		t.Fatal("recovery key should tolerate case and separators")
	}
}
