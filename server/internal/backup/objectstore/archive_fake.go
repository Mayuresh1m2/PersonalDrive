package objectstore

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ArchiveSim wraps a Store and simulates cold storage classes (Glacier /
// Deep Archive): objects put with an archive class can't be read until a
// restore is requested and then completed with CompleteRestores. Used by
// tests and the disaster drill, since MinIO has no archive tiers.
type ArchiveSim struct {
	Store
	mu       sync.Mutex
	state    map[string]RestoreState
	class    map[string]string
	Requests atomic.Int64 // restore requests
	GetBytes atomic.Int64 // bytes read (egress)
	Gets     atomic.Int64
	Puts     atomic.Int64
}

func NewArchiveSim(s Store) *ArchiveSim {
	return &ArchiveSim{Store: s, state: map[string]RestoreState{}, class: map[string]string{}}
}

func (a *ArchiveSim) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, opts PutOptions) error {
	if err := a.Store.Put(ctx, key, r, size, opts); err != nil {
		return err
	}
	a.Puts.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.class[key] = opts.StorageClass
	if archivedClass(opts.StorageClass) {
		a.state[key] = RestoreNone
	} else {
		delete(a.state, key)
	}
	return nil
}

func (a *ArchiveSim) readable(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.state[key]
	return !ok || st == RestoreReady
}

func (a *ArchiveSim) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if !a.readable(key) {
		return nil, fmt.Errorf("%w: %s", ErrArchived, key)
	}
	rc, err := a.Store.Get(ctx, key, off, n)
	if err != nil {
		return nil, err
	}
	a.Gets.Add(1)
	return &countingReader{ReadCloser: rc, n: &a.GetBytes}, nil
}

func (a *ArchiveSim) Head(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := a.Store.Head(ctx, key)
	if err != nil {
		return info, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	info.StorageClass = a.class[key]
	if st, ok := a.state[key]; ok {
		info.Restore = st
	} else {
		info.Restore = RestoreNotNeeded
	}
	return info, nil
}

func (a *ArchiveSim) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	return a.Store.List(ctx, prefix, func(o ObjectInfo) error {
		a.mu.Lock()
		o.StorageClass = a.class[o.Key]
		if st, ok := a.state[o.Key]; ok && st != RestoreReady {
			o.Restore = ""
		}
		a.mu.Unlock()
		return fn(o)
	})
}

func (a *ArchiveSim) Delete(ctx context.Context, keys ...string) error {
	a.mu.Lock()
	for _, k := range keys {
		delete(a.state, k)
		delete(a.class, k)
	}
	a.mu.Unlock()
	return a.Store.Delete(ctx, keys...)
}

func (a *ArchiveSim) RequestRestore(ctx context.Context, key, tier string, days int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state[key] == RestoreNone {
		a.state[key] = RestoreInProgress
		a.Requests.Add(1)
	}
	return nil
}

// CompleteRestores finishes every in-progress restore (simulates the wait).
func (a *ArchiveSim) CompleteRestores() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for k, st := range a.state {
		if st == RestoreInProgress {
			a.state[k] = RestoreReady
			n++
		}
	}
	return n
}

type countingReader struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}
