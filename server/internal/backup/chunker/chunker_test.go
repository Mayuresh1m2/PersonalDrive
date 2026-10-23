package chunker

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func chunks(t *testing.T, data []byte, p Params, g *Gear) [][]byte {
	t.Helper()
	c := New(bytes.NewReader(data), p, g)
	var out [][]byte
	for {
		b, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, append([]byte(nil), b...))
	}
}

func TestReassemblesAndRespectsBounds(t *testing.T) {
	data := make([]byte, 20<<20)
	rand.New(rand.NewSource(1)).Read(data)
	p := DefaultParams
	cs := chunks(t, data, p, NewGear([]byte("seed")))
	if !bytes.Equal(bytes.Join(cs, nil), data) {
		t.Fatal("chunks don't reassemble to the input")
	}
	for i, c := range cs {
		if len(c) > p.Max || (len(c) < p.Min && i != len(cs)-1) {
			t.Fatalf("chunk %d has size %d outside [%d,%d]", i, len(c), p.Min, p.Max)
		}
	}
	avg := len(data) / len(cs)
	if avg < p.Avg/2 || avg > p.Avg*2 {
		t.Errorf("average chunk size %d far from target %d", avg, p.Avg)
	}
}

func TestDeterministicAndShiftResistant(t *testing.T) {
	data := make([]byte, 16<<20)
	rand.New(rand.NewSource(2)).Read(data)
	g := NewGear([]byte("seed"))
	a := chunks(t, data, DefaultParams, g)
	b := chunks(t, data, DefaultParams, g)
	if len(a) != len(b) {
		t.Fatal("not deterministic")
	}
	// Insert bytes near the start: most chunks must survive.
	edited := append(append(append([]byte{}, data[:1000]...), []byte("inserted bytes!")...), data[1000:]...)
	c := chunks(t, edited, DefaultParams, g)
	seen := map[[32]byte]bool{}
	for _, x := range a {
		seen[sha256.Sum256(x)] = true
	}
	shared := 0
	for _, x := range c {
		if seen[sha256.Sum256(x)] {
			shared++
		}
	}
	if shared < len(a)-3 {
		t.Fatalf("only %d of %d chunks survived a small insert", shared, len(a))
	}
	// A different seed gives different boundaries.
	d := chunks(t, data, DefaultParams, NewGear([]byte("other")))
	if len(d) == len(a) && bytes.Equal(d[0], a[0]) {
		t.Error("seed has no effect on boundaries")
	}
}

func TestSmallAndEmpty(t *testing.T) {
	g := NewGear(nil)
	if cs := chunks(t, nil, DefaultParams, g); len(cs) != 0 {
		t.Fatal("empty input should give no chunks")
	}
	if cs := chunks(t, []byte("tiny"), DefaultParams, g); len(cs) != 1 || string(cs[0]) != "tiny" {
		t.Fatal("small input should be one chunk")
	}
}
