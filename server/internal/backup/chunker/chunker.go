// Package chunker implements FastCDC content-defined chunking.
//
// Chunk boundaries depend on content, not offsets, so inserting bytes near the
// start of a file only changes the chunks around the edit and the rest still
// dedup. The gear table is derived from a per-repository secret seed, so chunk
// boundaries (and therefore sizes) don't leak information about known files.
package chunker

import (
	"errors"
	"io"
	"math/bits"

	"github.com/zeebo/blake3"
)

type Params struct {
	Min, Avg, Max int
}

// DefaultParams suit photos and videos: ~1 MiB chunks keep index size small
// for multi-TB libraries while still deduping edited documents.
var DefaultParams = Params{Min: 256 << 10, Avg: 1 << 20, Max: 4 << 20}

func (p Params) Validate() error {
	if p.Min <= 0 || p.Min >= p.Avg || p.Avg >= p.Max || bits.OnesCount(uint(p.Avg)) != 1 {
		return errors.New("chunker: need 0 < min < avg < max and avg a power of two")
	}
	return nil
}

// Gear is the 256-entry random table driving the rolling hash.
type Gear [256]uint64

// NewGear derives a gear table deterministically from seed.
func NewGear(seed []byte) *Gear {
	h := blake3.New()
	h.Write([]byte("pdrive chunker gear v1"))
	h.Write(seed)
	d := h.Digest()
	var buf [8 * 256]byte
	d.Read(buf[:])
	var g Gear
	for i := range g {
		var v uint64
		for j := 0; j < 8; j++ {
			v = v<<8 | uint64(buf[i*8+j])
		}
		g[i] = v
	}
	return &g
}

// Chunker splits a stream into content-defined chunks.
type Chunker struct {
	r            io.Reader
	p            Params
	gear         *Gear
	maskS, maskL uint64
	buf          []byte
	start, end   int
	eof          bool
}

func New(r io.Reader, p Params, gear *Gear) *Chunker {
	avgBits := bits.TrailingZeros(uint(p.Avg))
	// Normalised chunking (level 2): stricter mask before the average size,
	// looser after, which tightens the size distribution around Avg.
	// Masks test the top bits of the hash, which mix the last ~64 bytes.
	top := func(n int) uint64 { return ^uint64(0) << (64 - n) }
	return &Chunker{
		r: r, p: p, gear: gear,
		maskS: top(avgBits + 2), maskL: top(avgBits - 2),
		buf: make([]byte, 2*p.Max),
	}
}

// Next returns the next chunk. The slice is only valid until the next call.
// It returns io.EOF after the last chunk.
func (c *Chunker) Next() ([]byte, error) {
	if err := c.fill(); err != nil {
		return nil, err
	}
	n := c.end - c.start
	if n == 0 {
		return nil, io.EOF
	}
	cut := c.cutpoint(c.buf[c.start:c.end])
	chunk := c.buf[c.start : c.start+cut]
	c.start += cut
	return chunk, nil
}

// fill ensures at least Max bytes are buffered unless the stream ended.
func (c *Chunker) fill() error {
	if c.eof || c.end-c.start >= c.p.Max {
		return nil
	}
	if c.start > 0 {
		copy(c.buf, c.buf[c.start:c.end])
		c.end -= c.start
		c.start = 0
	}
	for c.end < len(c.buf) && !c.eof {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if errors.Is(err, io.EOF) {
			c.eof = true
		} else if err != nil {
			return err
		}
		if c.end-c.start >= c.p.Max {
			break
		}
	}
	return nil
}

func (c *Chunker) cutpoint(data []byte) int {
	n := len(data)
	if n <= c.p.Min {
		return n
	}
	if n > c.p.Max {
		n = c.p.Max
	}
	normal := min(c.p.Avg, n)
	var fp uint64
	i := c.p.Min
	for ; i < normal; i++ {
		fp = (fp << 1) + c.gear[data[i]]
		if fp&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + c.gear[data[i]]
		if fp&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}
