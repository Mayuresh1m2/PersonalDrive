package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/crypto"
)

// Pack layout:
//
//	sealed blob 0 | sealed blob 1 | ... | sealed header | uint32 LE header length | "PDPK" | version
//
// Each sealed blob is Seal(codec byte || payload, aad = blob id). The header
// lists every blob (id, type, offset, length, raw length) so a pack is
// self-describing and the index can be rebuilt from packs if ever needed.
const (
	packMagic      = "PDPK"
	packVersion    = 1
	trailerSize    = 4 + len(packMagic) + 1
	headerEntrySz  = 32 + 1 + 4 + 4 + 4
	headerAAD      = "pdrive pack header v1"
	compressMinGap = 0.95 // keep compressed form only if it saves >= 5%
)

var (
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
)

// EncodeBlob compresses (when worthwhile) and seals plaintext.
func EncodeBlob(key *crypto.MasterKey, id BlobID, plain []byte) (sealed []byte, compressed bool) {
	payload := make([]byte, 0, len(plain)+1)
	if len(plain) >= 64 {
		z := zenc.EncodeAll(plain, make([]byte, 1, len(plain)/2+1))
		if float64(len(z)-1) <= float64(len(plain))*compressMinGap {
			z[0] = CodecZstd
			payload = z
			compressed = true
		}
	}
	if !compressed {
		payload = append(append(payload, CodecRaw), plain...)
	}
	return key.Seal(payload, id[:]), compressed
}

// DecodeBlob opens a sealed blob and verifies its id.
func DecodeBlob(key *crypto.MasterKey, id BlobID, sealed []byte) ([]byte, error) {
	payload, err := key.Open(sealed, id[:])
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", IDString(id)[:12], err)
	}
	if len(payload) == 0 {
		return nil, errors.New("empty blob payload")
	}
	var plain []byte
	switch payload[0] {
	case CodecRaw:
		plain = payload[1:]
	case CodecZstd:
		if plain, err = zdec.DecodeAll(payload[1:], nil); err != nil {
			return nil, fmt.Errorf("blob %s: decompress: %w", IDString(id)[:12], err)
		}
	default:
		return nil, fmt.Errorf("blob %s: unknown codec %d", IDString(id)[:12], payload[0])
	}
	if key.ID(plain) != id {
		return nil, fmt.Errorf("blob %s: content does not match its id", IDString(id)[:12])
	}
	return plain, nil
}

// PackWriter builds one pack in a local staging file.
type PackWriter struct {
	Hot     bool
	key     *crypto.MasterKey
	f       *os.File
	h       hash.Hash
	size    int64
	entries []PackEntry
}

func NewPackWriter(dir string, hot bool, key *crypto.MasterKey) (*PackWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "pack-"+uuid.NewString()))
	if err != nil {
		return nil, err
	}
	return &PackWriter{Hot: hot, key: key, f: f, h: sha256.New()}, nil
}

func (w *PackWriter) Size() int64 { return w.size }
func (w *PackWriter) Count() int  { return len(w.entries) }

func (w *PackWriter) write(b []byte) error {
	if _, err := w.f.Write(b); err != nil {
		return err
	}
	w.h.Write(b)
	w.size += int64(len(b))
	return nil
}

// Add appends a sealed blob.
func (w *PackWriter) Add(id BlobID, typ BlobType, sealed []byte, raw int) error {
	if w.size+int64(len(sealed)) > 1<<32-1 {
		return errors.New("pack too large")
	}
	e := PackEntry{ID: id, Type: typ, Offset: uint32(w.size), Length: uint32(len(sealed)), Raw: uint32(raw)}
	if err := w.write(sealed); err != nil {
		return err
	}
	w.entries = append(w.entries, e)
	return nil
}

// Finalize writes the header and trailer, closes the file and returns its
// path and description. The pack id is the SHA-256 of the file.
func (w *PackWriter) Finalize() (string, PackInfo, error) {
	hdr := make([]byte, 0, len(w.entries)*headerEntrySz)
	for _, e := range w.entries {
		hdr = append(hdr, e.ID[:]...)
		hdr = append(hdr, byte(e.Type))
		hdr = binary.LittleEndian.AppendUint32(hdr, e.Offset)
		hdr = binary.LittleEndian.AppendUint32(hdr, e.Length)
		hdr = binary.LittleEndian.AppendUint32(hdr, e.Raw)
	}
	sealed := w.key.Seal(hdr, []byte(headerAAD))
	trailer := binary.LittleEndian.AppendUint32(nil, uint32(len(sealed)))
	trailer = append(append(trailer, packMagic...), packVersion)
	err := w.write(sealed)
	if err == nil {
		err = w.write(trailer)
	}
	if err == nil {
		err = w.f.Sync()
	}
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(w.f.Name())
		return "", PackInfo{}, err
	}
	info := PackInfo{ID: hex.EncodeToString(w.h.Sum(nil)), Hot: w.Hot, Size: w.size, Blobs: w.entries}
	return w.f.Name(), info, nil
}

// Abort discards the staging file.
func (w *PackWriter) Abort() {
	w.f.Close()
	os.Remove(w.f.Name())
}

// ReadPackHeader parses a complete pack held in memory (used by check and
// index rebuild).
func ReadPackHeader(key *crypto.MasterKey, pack []byte) ([]PackEntry, error) {
	if len(pack) < trailerSize {
		return nil, errors.New("pack too short")
	}
	tr := pack[len(pack)-trailerSize:]
	if !bytes.Equal(tr[4:8], []byte(packMagic)) || tr[8] != packVersion {
		return nil, errors.New("not a pack file")
	}
	hlen := int(binary.LittleEndian.Uint32(tr[:4]))
	start := len(pack) - trailerSize - hlen
	if start < 0 {
		return nil, errors.New("corrupt pack trailer")
	}
	hdr, err := key.Open(pack[start:len(pack)-trailerSize], []byte(headerAAD))
	if err != nil {
		return nil, fmt.Errorf("pack header: %w", err)
	}
	if len(hdr)%headerEntrySz != 0 {
		return nil, errors.New("corrupt pack header")
	}
	out := make([]PackEntry, 0, len(hdr)/headerEntrySz)
	for i := 0; i < len(hdr); i += headerEntrySz {
		var e PackEntry
		copy(e.ID[:], hdr[i:i+32])
		e.Type = BlobType(hdr[i+32])
		e.Offset = binary.LittleEndian.Uint32(hdr[i+33:])
		e.Length = binary.LittleEndian.Uint32(hdr[i+37:])
		e.Raw = binary.LittleEndian.Uint32(hdr[i+41:])
		out = append(out, e)
	}
	return out, nil
}

// VerifyPackID checks a downloaded pack's bytes against its id.
func VerifyPackID(id string, r io.Reader) error {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != id {
		return fmt.Errorf("pack %s is corrupt (hash %s)", id[:12], got[:12])
	}
	return nil
}
