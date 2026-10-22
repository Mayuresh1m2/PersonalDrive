// Package processors runs pluggable per-file work (thumbnails, metadata
// extraction now; AI features later) from the durable job queue.
//
// Work is keyed by content hash and processor version: duplicate files are
// processed once, and bumping Version() reprocesses everything.
package processors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/events"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/jobs"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
)

// FileInfo is what a processor gets to look at.
type FileInfo struct {
	SHA256  string
	Mime    string
	Name    string
	Size    int64
	AbsPath string
}

type Processor interface {
	Name() string
	Version() int
	Accepts(f FileInfo) bool
	Process(ctx context.Context, f FileInfo, src *os.File) error
}

// ErrUnsupported marks content a processor can't handle (recorded, not retried).
var ErrUnsupported = errors.New("unsupported content")

const jobKind = "process"

type payload struct {
	Processor string `json:"processor"`
	SHA256    string `json:"sha256"`
	Mime      string `json:"mime"`
}

// Runner wires processors to the event bus and job queue.
type Runner struct {
	db    *metadata.DB
	queue *jobs.Queue
	procs map[string]Processor
	path  func(ctx context.Context, f *metadata.File) (string, error)
	log   *slog.Logger
}

func NewRunner(db *metadata.DB, queue *jobs.Queue, absPath func(context.Context, *metadata.File) (string, error), log *slog.Logger, procs ...Processor) *Runner {
	r := &Runner{db: db, queue: queue, procs: map[string]Processor{}, path: absPath, log: log}
	for _, p := range procs {
		r.procs[p.Name()] = p
	}
	queue.Handle(jobKind, r.handle)
	return r
}

// Subscribe enqueues processing for new and changed content.
func (r *Runner) Subscribe(bus *events.Bus) {
	h := func(ctx context.Context, e events.Event) error {
		if e.SHA256 == "" {
			return nil
		}
		return r.Enqueue(ctx, e.SHA256, e.Mime, "")
	}
	bus.Subscribe(events.FileCreated, h)
	bus.Subscribe(events.FileUpdated, h)
}

// Enqueue schedules every accepting processor (or just `only`) for content.
func (r *Runner) Enqueue(ctx context.Context, sha, mime, only string) error {
	for name, p := range r.procs {
		if only != "" && name != only {
			continue
		}
		if !p.Accepts(FileInfo{SHA256: sha, Mime: mime}) {
			continue
		}
		key := fmt.Sprintf("%s:v%d:%s", name, p.Version(), sha)
		if err := r.queue.Enqueue(ctx, jobKind, key, payload{Processor: name, SHA256: sha, Mime: mime}, false); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) handle(ctx context.Context, j jobs.Job) error {
	var pl payload
	if err := json.Unmarshal(j.Payload, &pl); err != nil {
		return fmt.Errorf("%w: %v", jobs.ErrPermanent, err)
	}
	p, ok := r.procs[pl.Processor]
	if !ok {
		return fmt.Errorf("%w: unknown processor %s", jobs.ErrPermanent, pl.Processor)
	}
	if st, err := r.db.ProcessorState(ctx, p.Name(), pl.SHA256); err == nil && st.Version >= p.Version() && st.Status != "failed" {
		return nil
	}
	files, err := r.db.AnyActiveBySHA(ctx, pl.SHA256, 1)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil // content deleted before we got to it
	}
	abs, err := r.path(ctx, files[0])
	if err != nil {
		return err
	}
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	fi := FileInfo{SHA256: pl.SHA256, Mime: files[0].Mime, Name: files[0].Name, Size: files[0].Size, AbsPath: abs}
	perr := p.Process(ctx, fi, src)
	switch {
	case perr == nil:
		return r.db.SetProcessorState(ctx, p.Name(), pl.SHA256, p.Version(), "done", "")
	case errors.Is(perr, ErrUnsupported):
		return r.db.SetProcessorState(ctx, p.Name(), pl.SHA256, p.Version(), "unsupported", perr.Error())
	default:
		if j.Attempts >= 5 || errors.Is(perr, jobs.ErrPermanent) {
			r.db.SetProcessorState(ctx, p.Name(), pl.SHA256, p.Version(), "failed", perr.Error())
		}
		return perr
	}
}
