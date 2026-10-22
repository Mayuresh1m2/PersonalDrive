// Package jobs is a durable SQLite-backed job queue with a worker pool.
// Jobs are unique per (kind, key), so enqueueing the same work twice is a no-op.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const maxAttempts = 5

type Job struct {
	ID       int64
	Kind     string
	Key      string
	Payload  json.RawMessage
	Attempts int
}

// Handler runs a job. Returning ErrPermanent (wrapped) stops retries.
type Handler func(ctx context.Context, j Job) error

var ErrPermanent = errors.New("permanent failure")

type Queue struct {
	db       *sql.DB
	log      *slog.Logger
	handlers map[string]Handler
	wake     chan struct{}
}

func New(db *sql.DB, log *slog.Logger) *Queue {
	return &Queue{db: db, log: log, handlers: map[string]Handler{}, wake: make(chan struct{}, 1)}
}

// Handle registers the handler for a job kind. Call before Run.
func (q *Queue) Handle(kind string, h Handler) { q.handlers[kind] = h }

// Enqueue adds a job unless one with the same kind and key already exists.
// If the existing job failed or finished, it is reset only when reset is true.
func (q *Queue) Enqueue(ctx context.Context, kind, key string, payload any, reset bool) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	stmt := `INSERT INTO jobs (kind, key, payload, run_after, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, key) DO NOTHING`
	if reset {
		stmt = `INSERT INTO jobs (kind, key, payload, run_after, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, key) DO UPDATE SET payload = excluded.payload, state = 'pending', attempts = 0,
		  run_after = excluded.run_after, last_error = NULL, updated_at = excluded.updated_at
		WHERE jobs.state IN ('done', 'failed')`
	}
	if _, err := q.db.ExecContext(ctx, stmt, kind, key, string(body), now, now, now); err != nil {
		return err
	}
	q.signal()
	return nil
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Stats counts jobs by state.
func (q *Queue) Stats(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// Run processes jobs with n workers until ctx is cancelled.
func (q *Queue) Run(ctx context.Context, n int) {
	// Jobs left running by a crash are retried.
	q.db.ExecContext(ctx, `UPDATE jobs SET state = 'pending' WHERE state = 'running'`)
	// Keep the table small: finished jobs older than a week are dropped.
	q.db.ExecContext(ctx, `DELETE FROM jobs WHERE state = 'done' AND updated_at < ?`, time.Now().Add(-7*24*time.Hour).UnixMilli())

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.worker(ctx)
		}()
	}
	wg.Wait()
}

func (q *Queue) worker(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		for {
			j, err := q.claim(ctx)
			if err != nil {
				if ctx.Err() == nil {
					q.log.Error("claim job", "err", err)
				}
				break
			}
			if j == nil {
				break
			}
			q.run(ctx, j)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-tick.C:
		}
	}
}

func (q *Queue) claim(ctx context.Context) (*Job, error) {
	var j Job
	var payload string
	err := q.db.QueryRowContext(ctx,
		`UPDATE jobs SET state = 'running', attempts = attempts + 1, updated_at = ?
		 WHERE id = (SELECT id FROM jobs WHERE state = 'pending' AND run_after <= ? ORDER BY id LIMIT 1)
		 RETURNING id, kind, key, payload, attempts`,
		time.Now().UnixMilli(), time.Now().UnixMilli()).Scan(&j.ID, &j.Kind, &j.Key, &payload, &j.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Payload = json.RawMessage(payload)
	return &j, nil
}

func (q *Queue) run(ctx context.Context, j *Job) {
	h, ok := q.handlers[j.Kind]
	var err error
	if !ok {
		err = fmt.Errorf("%w: no handler for job kind %q", ErrPermanent, j.Kind)
	} else {
		err = safeCall(ctx, h, *j)
	}
	now := time.Now()
	switch {
	case err == nil:
		q.db.ExecContext(ctx, `UPDATE jobs SET state = 'done', last_error = NULL, updated_at = ? WHERE id = ?`, now.UnixMilli(), j.ID)
	case errors.Is(err, ErrPermanent) || j.Attempts >= maxAttempts:
		q.log.Warn("job failed", "kind", j.Kind, "key", j.Key, "attempts", j.Attempts, "err", err)
		q.db.ExecContext(ctx, `UPDATE jobs SET state = 'failed', last_error = ?, updated_at = ? WHERE id = ?`, err.Error(), now.UnixMilli(), j.ID)
	default:
		backoff := time.Duration(1<<j.Attempts) * 10 * time.Second
		q.db.ExecContext(ctx, `UPDATE jobs SET state = 'pending', last_error = ?, run_after = ?, updated_at = ? WHERE id = ?`,
			err.Error(), now.Add(backoff).UnixMilli(), now.UnixMilli(), j.ID)
	}
}

func safeCall(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: panic: %v", ErrPermanent, r)
		}
	}()
	return h(ctx, j)
}
