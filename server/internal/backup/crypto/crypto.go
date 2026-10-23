// Package crypto holds backup repository keys.
//
// A repository has one random master key (an encryption key plus a separate
// key for chunk ids). Key files wrap the master key under a key derived with
// Argon2id from a secret: the owner's passphrase, or the printed recovery key.
// Losing every secret means losing the backup; there is no back door.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeebo/blake3"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

var ErrWrongSecret = errors.New("wrong passphrase or recovery key")

// BlobID is the keyed BLAKE3 hash of a blob's plaintext.
type BlobID [32]byte

type MasterKey struct {
	Enc [32]byte `json:"enc"`
	Mac [32]byte `json:"mac"`
}

func NewMasterKey() (*MasterKey, error) {
	var k MasterKey
	if _, err := rand.Read(k.Enc[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(k.Mac[:]); err != nil {
		return nil, err
	}
	return &k, nil
}

// ID returns the content id of plaintext. Keyed, so ids reveal nothing about
// content to someone without the key.
func (k *MasterKey) ID(plaintext []byte) BlobID {
	h, _ := blake3.NewKeyed(k.Mac[:])
	h.Write(plaintext)
	var id BlobID
	copy(id[:], h.Sum(nil))
	return id
}

// Seal encrypts with a random nonce: nonce || ciphertext+tag.
func (k *MasterKey) Seal(plaintext, aad []byte) []byte {
	return seal(k.Enc[:], plaintext, aad)
}

func (k *MasterKey) Open(sealed, aad []byte) ([]byte, error) {
	return open(k.Enc[:], sealed, aad)
}

// Overhead is the bytes Seal adds.
const Overhead = chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

func seal(key, plaintext, aad []byte) []byte {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return aead.Seal(nonce, nonce, plaintext, aad)
}

func open(key, sealed, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	out, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("decryption failed: data corrupted or wrong key")
	}
	return out, nil
}

// Key file labels.
const (
	LabelPassphrase = "passphrase"
	LabelRecovery   = "recovery"
)

// KeyFile is stored (unencrypted JSON) at keys/<id> in the repository.
type KeyFile struct {
	Version int       `json:"version"`
	Label   string    `json:"label"` // "passphrase" or "recovery"
	KDF     string    `json:"kdf"`
	Time    uint32    `json:"time"`
	Memory  uint32    `json:"memory_kib"`
	Threads uint8     `json:"threads"`
	Salt    []byte    `json:"salt"`
	Sealed  []byte    `json:"sealed"` // master key sealed under the derived key
	Created time.Time `json:"created"`
}

// KDFParams for passphrases: ~0.5–1s on a laptop, fine on a Pi.
var KDFParams = struct {
	Time, Memory uint32
	Threads      uint8
}{Time: 3, Memory: 128 * 1024, Threads: 4}

// Wrap seals the master key under a key derived from secret.
func Wrap(mk *MasterKey, secret, label string) (*KeyFile, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	kf := &KeyFile{
		Version: 1, Label: label, KDF: "argon2id",
		Time: KDFParams.Time, Memory: KDFParams.Memory, Threads: KDFParams.Threads,
		Salt: salt, Created: time.Now().UTC(),
	}
	plain := append(append([]byte{}, mk.Enc[:]...), mk.Mac[:]...)
	kf.Sealed = seal(kf.derive(secret), plain, []byte("pdrive key v1"))
	return kf, nil
}

func (kf *KeyFile) derive(secret string) []byte {
	if kf.Label == LabelRecovery {
		secret = normalizeRecovery(secret)
	}
	return argon2.IDKey([]byte(secret), kf.Salt, kf.Time, kf.Memory, kf.Threads, 32)
}

// Unwrap recovers the master key with the secret.
func (kf *KeyFile) Unwrap(secret string) (*MasterKey, error) {
	if kf.KDF != "argon2id" {
		return nil, fmt.Errorf("unsupported KDF %q", kf.KDF)
	}
	plain, err := open(kf.derive(secret), kf.Sealed, []byte("pdrive key v1"))
	if err != nil || len(plain) != 64 {
		return nil, ErrWrongSecret
	}
	var mk MasterKey
	copy(mk.Enc[:], plain[:32])
	copy(mk.Mac[:], plain[32:])
	return &mk, nil
}

// Equal compares two master keys in constant time.
func (k *MasterKey) Equal(o *MasterKey) bool {
	return subtle.ConstantTimeCompare(k.Enc[:], o.Enc[:]) == 1 && subtle.ConstantTimeCompare(k.Mac[:], o.Mac[:]) == 1
}

// NewRecoveryKey returns a random key formatted for printing:
// 52 base32 characters in groups of four (260 bits).
func NewRecoveryKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	var groups []string
	for i := 0; i < len(s); i += 4 {
		groups = append(groups, s[i:min(i+4, len(s))])
	}
	return strings.Join(groups, "-"), nil
}

// normalizeRecovery makes recovery keys tolerant of case, spaces and dashes.
// Passphrases are used exactly as typed.
func normalizeRecovery(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\n", "").Replace(strings.TrimSpace(s)))
}
