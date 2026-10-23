// Package events is a small in-process event bus. Handlers must be quick
// (enqueue a job, wake a waiter); slow work belongs in the job queue.
package events

import (
	"context"
	"log/slog"
	"sync"
)

// Topics.
const (
	FileCreated = "file.created"
	FileUpdated = "file.updated"
	FileMoved   = "file.moved"
	FileTrashed = "file.trashed"
	FileRestore = "file.restored"
	FileDeleted = "file.deleted"
)

type Event struct {
	Topic   string
	FileID  int64
	SHA256  string
	Mime    string
	Seq     int64
	Owner   string // "user:3" / "space:1"
	Payload map[string]any
}

type Handler func(context.Context, Event) error

type Bus struct {
	mu       sync.RWMutex
	handlers map[string]map[int]Handler
	next     int
	log      *slog.Logger
}

func NewBus(log *slog.Logger) *Bus {
	return &Bus{handlers: make(map[string]map[int]Handler), log: log}
}

// Subscribe registers h for a topic ("*" for all) and returns an unsubscribe func.
func (b *Bus) Subscribe(topic string, h Handler) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handlers[topic] == nil {
		b.handlers[topic] = make(map[int]Handler)
	}
	id := b.next
	b.next++
	b.handlers[topic][id] = h
	return func() {
		b.mu.Lock()
		delete(b.handlers[topic], id)
		b.mu.Unlock()
	}
}

// Publish delivers e to topic and wildcard subscribers. Handler errors are
// logged, not returned: a failing subscriber must not fail the write that
// produced the event.
func (b *Bus) Publish(ctx context.Context, e Event) {
	b.mu.RLock()
	var hs []Handler
	for _, h := range b.handlers[e.Topic] {
		hs = append(hs, h)
	}
	for _, h := range b.handlers["*"] {
		hs = append(hs, h)
	}
	b.mu.RUnlock()
	for _, h := range hs {
		if err := h(ctx, e); err != nil && b.log != nil {
			b.log.Warn("event handler failed", "topic", e.Topic, "err", err)
		}
	}
}

// Notifier lets goroutines wait until the change sequence passes a value.
type Notifier struct {
	mu   sync.Mutex
	seq  int64
	wake chan struct{}
}

func NewNotifier(seq int64) *Notifier {
	return &Notifier{seq: seq, wake: make(chan struct{})}
}

// Advance records a new sequence number and wakes waiters.
func (n *Notifier) Advance(seq int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if seq <= n.seq {
		return
	}
	n.seq = seq
	close(n.wake)
	n.wake = make(chan struct{})
}

func (n *Notifier) Seq() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.seq
}

// Wait blocks until the sequence exceeds after or ctx is done, and returns
// the current sequence.
func (n *Notifier) Wait(ctx context.Context, after int64) int64 {
	for {
		n.mu.Lock()
		seq, ch := n.seq, n.wake
		n.mu.Unlock()
		if seq > after {
			return seq
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return n.Seq()
		}
	}
}
