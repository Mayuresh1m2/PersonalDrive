// Package objectstore abstracts where backup repositories live: a local disk
// (disk B) or S3 / any S3-compatible service (Backblaze B2, Cloudflare R2,
// Wasabi, MinIO).
package objectstore

import (
	"context"
	"errors"
	"io"
	"time"

	"golang.org/x/time/rate"
)

var (
	ErrNotFound = errors.New("object not found")
	// ErrArchived means the object is in a cold tier (Glacier/Deep Archive)
	// and must be restored before it can be read.
	ErrArchived = errors.New("object is archived; request a restore first")
)

type RestoreState string

const (
	RestoreNotNeeded  RestoreState = "not_needed"  // readable now
	RestoreNone       RestoreState = "none"        // archived, no restore requested
	RestoreInProgress RestoreState = "in_progress" // thawing
	RestoreReady      RestoreState = "ready"       // temporary copy readable
)

type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	StorageClass string
	Restore      RestoreState
}

// Readable reports whether Get will succeed without a restore.
func (o ObjectInfo) Readable() bool {
	return o.Restore == RestoreNotNeeded || o.Restore == RestoreReady || o.Restore == ""
}

type PutOptions struct {
	StorageClass string // "" = the store's default / STANDARD
}

type Store interface {
	// Put uploads size bytes from r. r is seekable so failed attempts can retry.
	Put(ctx context.Context, key string, r io.ReadSeeker, size int64, opts PutOptions) error
	// Get reads length bytes at offset (length < 0 reads to the end).
	Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	// List calls fn for every object under prefix. Restore state may be
	// unknown ("") in listings; use Head for that.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
	Delete(ctx context.Context, keys ...string) error
	// RequestRestore starts thawing an archived object for `days` days.
	// tier: "Bulk" (cheapest, up to 48h on Deep Archive) | "Standard" | "Expedited".
	// It is a no-op for readable objects and restores already in progress.
	RequestRestore(ctx context.Context, key, tier string, days int) error
	// Describe is a human-readable location, e.g. "s3://bucket/prefix".
	Describe() string
}

// GetAll reads a whole object into memory.
func GetAll(ctx context.Context, s Store, key string) ([]byte, error) {
	rc, err := s.Get(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// rateLimitedReader throttles reads to stay under a bandwidth cap.
type rateLimitedReader struct {
	ctx context.Context
	r   io.ReadSeeker
	l   *rate.Limiter
}

func throttle(ctx context.Context, r io.ReadSeeker, l *rate.Limiter) io.ReadSeeker {
	if l == nil {
		return r
	}
	return &rateLimitedReader{ctx: ctx, r: r, l: l}
}

func (t *rateLimitedReader) Read(p []byte) (int, error) {
	if len(p) > t.l.Burst() {
		p = p[:t.l.Burst()]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		if werr := t.l.WaitN(t.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (t *rateLimitedReader) Seek(off int64, whence int) (int64, error) { return t.r.Seek(off, whence) }

// NewLimiter returns a limiter for bytesPerSec (nil if unlimited).
func NewLimiter(bytesPerSec int64) *rate.Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := int(min(bytesPerSec, 1<<20))
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}
