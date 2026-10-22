// Package service runs backups inside the server: it opens configured
// targets, snapshots the library and server metadata on a schedule, prunes,
// checks, keeps status, and sends alerts.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/engine"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
)

// Source names inside snapshots.
const (
	SourceLibrary = "library"
	SourceMeta    = "meta"
)

// RestoreMarker in the state dir keeps the server read-only during a restore.
const RestoreMarker = "RESTORE_IN_PROGRESS"

type TargetStatus struct {
	Name         string              `json:"name"`
	Type         string              `json:"type"`
	Location     string              `json:"location"`
	StorageClass string              `json:"storage_class,omitempty"`
	Schedule     string              `json:"schedule"`
	NextRun      *time.Time          `json:"next_run,omitempty"`
	Running      string              `json:"running,omitempty"` // backup | prune | check
	Progress     *engine.Progress    `json:"progress,omitempty"`
	LastRun      *time.Time          `json:"last_run,omitempty"`
	LastSuccess  *time.Time          `json:"last_success,omitempty"`
	LastError    string              `json:"last_error,omitempty"`
	LastSnapshot string              `json:"last_snapshot,omitempty"`
	LastStats    *repo.SnapshotStats `json:"last_stats,omitempty"`
	LastPrune    *engine.PruneReport `json:"last_prune,omitempty"`
	LastCheck    *engine.CheckReport `json:"last_check,omitempty"`
	LastCheckAt  *time.Time          `json:"last_check_at,omitempty"`
	Initialized  bool                `json:"initialized"`
}

type Service struct {
	Cfg     *config.Config
	DB      *metadata.DB
	Log     *slog.Logger
	Program string
	Pricing engine.Pricing

	runMu  sync.Mutex // one heavy operation at a time (they read the same disks)
	mu     sync.Mutex
	status map[string]*TargetStatus
	cron   *cron.Cron
	ids    map[string]cron.EntryID
	http   *http.Client
}

func New(cfg *config.Config, db *metadata.DB, log *slog.Logger, program string) *Service {
	s := &Service{
		Cfg: cfg, DB: db, Log: log, Program: program, Pricing: engine.DefaultPricing(),
		status: map[string]*TargetStatus{}, ids: map[string]cron.EntryID{},
		http: &http.Client{Timeout: 15 * time.Second},
	}
	for _, t := range cfg.Backup.Targets {
		st := &TargetStatus{Name: t.Name, Type: t.Type, Schedule: t.Schedule, StorageClass: t.StorageClass}
		if t.Type == "local" {
			st.Location = t.Path
		} else {
			st.Location = "s3://" + t.Bucket + "/" + strings.Trim(t.Prefix, "/")
		}
		if data, err := db.Setting(context.Background(), "backup.status."+t.Name); err == nil && data != "" {
			var saved TargetStatus
			if json.Unmarshal([]byte(data), &saved) == nil {
				st.LastRun, st.LastSuccess, st.LastError = saved.LastRun, saved.LastSuccess, saved.LastError
				st.LastSnapshot, st.LastStats, st.LastPrune = saved.LastSnapshot, saved.LastStats, saved.LastPrune
				st.LastCheck, st.LastCheckAt, st.Initialized = saved.LastCheck, saved.LastCheckAt, saved.Initialized
			}
		}
		s.status[t.Name] = st
	}
	return s
}

// Passphrase reads the stored backup passphrase.
func (s *Service) Passphrase() (string, error) {
	if v := os.Getenv("PDRIVE_BACKUP_PASSPHRASE"); v != "" {
		return v, nil
	}
	data, err := os.ReadFile(s.Cfg.PassphrasePath())
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no backup passphrase at %s (run `pdrive backup init <target>`)", s.Cfg.PassphrasePath())
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// SavePassphrase stores the passphrase with 0600 permissions.
func SavePassphrase(path, pass string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(pass+"\n"), 0o600)
}

// OpenStore connects to a target's storage.
func OpenStore(ctx context.Context, t *config.BackupTarget) (objectstore.Store, error) {
	switch t.Type {
	case "local":
		return objectstore.OpenLocal(t.Path)
	case "s3":
		return objectstore.OpenS3(ctx, objectstore.S3Config{
			Bucket: t.Bucket, Prefix: t.Prefix, Region: t.Region, Endpoint: t.Endpoint,
			AccessKeyID: t.AccessKeyID, SecretAccessKey: t.SecretAccessKey, PathStyle: t.PathStyle,
			BandwidthLimit: t.BandwidthLimit,
		})
	}
	return nil, fmt.Errorf("unknown target type %q", t.Type)
}

// Classes returns hot/cold storage classes for a target.
func Classes(t *config.BackupTarget) repo.Classes {
	if t.Type == "local" {
		return repo.Classes{}
	}
	return repo.Classes{Hot: "STANDARD", Cold: t.StorageClass}
}

// OpenRepo opens a target's repository with the stored passphrase.
func (s *Service) OpenRepo(ctx context.Context, name string) (*repo.Repo, *config.BackupTarget, error) {
	t, err := s.Cfg.Target(name)
	if err != nil {
		return nil, nil, err
	}
	pass, err := s.Passphrase()
	if err != nil {
		return nil, nil, err
	}
	store, err := OpenStore(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	r, err := repo.Open(ctx, store, pass, s.Cfg.StatePath("backup", "cache"), Classes(t))
	if err != nil {
		return nil, nil, err
	}
	return r, t, nil
}

// Sources returns what a snapshot contains: the library (cold) and a
// consistent copy of server metadata (hot, so it restores fast).
func (s *Service) Sources(metaDir string) []engine.Source {
	return []engine.Source{
		{Name: SourceLibrary, Path: s.Cfg.Paths.Library, Exclude: []string{".tmp"}},
		{Name: SourceMeta, Path: metaDir, Hot: true},
	}
}

// PrepareMeta writes a consistent copy of the database, the server secrets
// (token signing key, TLS CA so paired devices keep trusting a restored
// server) and the config into dir. The backup passphrase is NOT included.
func (s *Service) PrepareMeta(ctx context.Context, dir string) error {
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := s.DB.SQL().ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(dir, "pdrive.db")); err != nil {
		return fmt.Errorf("snapshotting database: %w", err)
	}
	copyFile := func(src, dst string) error {
		data, err := os.ReadFile(src)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(dst), 0o700)
		return os.WriteFile(dst, data, 0o600)
	}
	for _, rel := range []string{"secrets/token.key", "tls/ca.pem", "tls/ca.key"} {
		if err := copyFile(s.Cfg.StatePath(rel), filepath.Join(dir, rel)); err != nil {
			return err
		}
	}
	if f := s.Cfg.File(); f != "" {
		if err := copyFile(f, filepath.Join(dir, "config.yaml")); err != nil {
			return err
		}
	}
	return nil
}

// ---- Operations -------------------------------------------------------------

// lockBackups serialises heavy work across the server and CLI processes.
func (s *Service) lockBackups(ctx context.Context) (func(), error) {
	s.runMu.Lock()
	f, err := os.OpenFile(s.Cfg.StatePath("backup.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		s.runMu.Unlock()
		return nil, err
	}
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			f.Close()
			s.runMu.Unlock()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		s.runMu.Unlock()
	}, nil
}

func (s *Service) setRunning(name, what string, p *engine.Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[name]; st != nil {
		st.Running, st.Progress = what, p
	}
}

func (s *Service) saveStatus(name string) {
	s.mu.Lock()
	st := *s.status[name]
	s.mu.Unlock()
	st.Running, st.Progress, st.NextRun = "", nil, nil
	data, _ := json.Marshal(st)
	if err := s.DB.SetSetting(context.Background(), "backup.status."+name, string(data)); err != nil {
		s.Log.Warn("saving backup status", "err", err)
	}
}

// Backup runs one backup of a target now, then prunes per its retention.
func (s *Service) Backup(ctx context.Context, name string, progress func(engine.Progress)) (*repo.Snapshot, error) {
	unlock, err := s.lockBackups(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.setRunning(name, "backup", &engine.Progress{Phase: "starting"})
	start := time.Now().UTC()
	snap, err := s.backup(ctx, name, progress)
	s.mu.Lock()
	st := s.status[name]
	if st == nil {
		s.mu.Unlock()
		return snap, err
	}
	st.LastRun = &start
	if err != nil {
		st.LastError = err.Error()
	} else {
		now := time.Now().UTC()
		st.LastSuccess, st.LastError, st.LastSnapshot, st.Initialized = &now, "", snap.ID, true
		stats := snap.Stats
		st.LastStats = &stats
	}
	s.mu.Unlock()
	s.setRunning(name, "", nil)
	s.saveStatus(name)
	if err != nil {
		s.notify(ctx, fmt.Sprintf("Backup to %s FAILED", name), err.Error())
	}
	return snap, err
}

func (s *Service) backup(ctx context.Context, name string, progress func(engine.Progress)) (*repo.Snapshot, error) {
	r, _, err := s.OpenRepo(ctx, name)
	if err != nil {
		return nil, err
	}
	metaDir := s.Cfg.StatePath("backup", "meta-staging")
	if err := s.PrepareMeta(ctx, metaDir); err != nil {
		return nil, err
	}
	defer os.RemoveAll(metaDir)
	snap, err := engine.Backup(ctx, r, engine.BackupOptions{
		Sources:    s.Sources(metaDir),
		StagingDir: s.Cfg.StatePath("backup", "staging", name),
		Program:    s.Program,
		Progress: func(p engine.Progress) {
			s.setRunning(name, "backup", &p)
			if progress != nil {
				progress(p)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	s.Log.Info("backup finished", "target", name, "snapshot", snap.ID[:12], "files", snap.Stats.Files,
		"new_mb", snap.Stats.BytesNew>>20, "deduped_mb", snap.Stats.BytesDeduped>>20,
		"uploaded_mb", snap.Stats.BytesUploaded>>20, "took", snap.Stats.Duration.Round(time.Second))
	if len(snap.Stats.Errors) > 0 {
		s.notify(ctx, fmt.Sprintf("Backup to %s skipped %d files", name, len(snap.Stats.Errors)), strings.Join(snap.Stats.Errors[:min(5, len(snap.Stats.Errors))], "\n"))
	}
	return snap, nil
}

// Prune applies the target's retention policy.
func (s *Service) Prune(ctx context.Context, name string, dryRun bool) (*engine.PruneReport, error) {
	unlock, err := s.lockBackups(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.setRunning(name, "prune", nil)
	defer s.setRunning(name, "", nil)
	r, t, err := s.OpenRepo(ctx, name)
	if err != nil {
		return nil, err
	}
	rep, err := engine.Prune(ctx, r, engine.PruneOptions{
		Retention: engine.Retention(t.Retention), DryRun: dryRun, Pricing: s.Pricing,
		StagingDir: s.Cfg.StatePath("backup", "staging", name+"-prune"),
	})
	if err != nil {
		s.notify(ctx, fmt.Sprintf("Prune of %s FAILED", name), err.Error())
		return nil, err
	}
	if !dryRun {
		s.mu.Lock()
		s.status[name].LastPrune = rep
		s.mu.Unlock()
		s.saveStatus(name)
	}
	return rep, nil
}

// Check verifies a target. Local targets read 5% of packs; S3 targets only
// check structure (reading Deep Archive data costs money) plus compare the
// live library against the latest snapshot.
func (s *Service) Check(ctx context.Context, name string, opts engine.CheckOptions) (*engine.CheckReport, error) {
	unlock, err := s.lockBackups(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.setRunning(name, "check", nil)
	defer s.setRunning(name, "", nil)
	r, _, err := s.OpenRepo(ctx, name)
	if err != nil {
		return nil, err
	}
	rep, err := engine.Check(ctx, r, opts)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.status[name].LastCheck, s.status[name].LastCheckAt = rep, &now
	s.mu.Unlock()
	s.saveStatus(name)
	if !rep.OK() {
		msg := strings.Join(append(rep.Errors[:min(5, len(rep.Errors))], rep.LocalMismatch[:min(5, len(rep.LocalMismatch))]...), "\n")
		s.notify(ctx, fmt.Sprintf("Backup check of %s found problems", name), msg)
	}
	return rep, nil
}

// ScheduledCheckOptions are what the scheduler runs for a target type.
func (s *Service) ScheduledCheckOptions(t *config.BackupTarget) engine.CheckOptions {
	o := engine.CheckOptions{LocalSource: SourceLibrary, LocalPath: s.Cfg.Paths.Library}
	if t.Type == "local" {
		o.ReadFraction = 0.05
	}
	return o
}

// ---- Scheduling ---------------------------------------------------------------

// Start schedules backups, prunes and checks for every target, and an
// hourly staleness alert.
func (s *Service) Start(ctx context.Context) error {
	s.cron = cron.New(cron.WithLocation(time.Local))
	for i := range s.Cfg.Backup.Targets {
		t := &s.Cfg.Backup.Targets[i]
		name := t.Name
		id, err := s.cron.AddFunc(t.Schedule, func() {
			if _, err := s.Backup(ctx, name, nil); err != nil {
				s.Log.Error("scheduled backup failed", "target", name, "err", err)
				return
			}
		})
		if err != nil {
			return fmt.Errorf("target %s: bad schedule %q: %w", name, t.Schedule, err)
		}
		s.ids[name] = id
		if _, err := s.cron.AddFunc(t.PruneSchedule, func() {
			if _, err := s.Prune(ctx, name, false); err != nil {
				s.Log.Error("scheduled prune failed", "target", name, "err", err)
			}
		}); err != nil {
			return fmt.Errorf("target %s: bad prune_schedule: %w", name, err)
		}
		if _, err := s.cron.AddFunc(t.CheckSchedule, func() {
			if _, err := s.Check(ctx, name, s.ScheduledCheckOptions(t)); err != nil {
				s.Log.Error("scheduled check failed", "target", name, "err", err)
			}
		}); err != nil {
			return fmt.Errorf("target %s: bad check_schedule: %w", name, err)
		}
	}
	s.cron.AddFunc("17 * * * *", func() { s.alertStale(ctx) })
	s.cron.Start()
	go func() {
		<-ctx.Done()
		s.cron.Stop()
	}()
	return nil
}

// RunAsync starts a backup in the background (for the API).
func (s *Service) RunAsync(name string) error {
	if _, err := s.Cfg.Target(name); err != nil {
		return err
	}
	s.mu.Lock()
	running := s.status[name].Running != ""
	s.mu.Unlock()
	if running {
		return fmt.Errorf("target %s is busy", name)
	}
	go func() {
		if _, err := s.Backup(context.Background(), name, nil); err != nil {
			s.Log.Error("backup failed", "target", name, "err", err)
		}
	}()
	return nil
}

// Status returns every target's status.
func (s *Service) Status() []TargetStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TargetStatus, 0, len(s.status))
	for _, t := range s.Cfg.Backup.Targets {
		st := *s.status[t.Name]
		if s.cron != nil {
			if e := s.cron.Entry(s.ids[t.Name]); e.Valid() {
				next := e.Next
				st.NextRun = &next
			}
		}
		out = append(out, st)
	}
	return out
}

// Warnings returns health warnings about backups.
func (s *Service) Warnings() []string {
	var out []string
	if len(s.Cfg.Backup.Targets) == 0 {
		return []string{"no backup targets configured — your library has no backup"}
	}
	stale := s.Cfg.Backup.Notify.StaleAfter.D()
	for _, st := range s.Status() {
		switch {
		case st.LastSuccess == nil:
			out = append(out, fmt.Sprintf("backup target %s has never completed a backup", st.Name))
		case stale > 0 && time.Since(*st.LastSuccess) > stale:
			out = append(out, fmt.Sprintf("last successful backup to %s was %s ago", st.Name, time.Since(*st.LastSuccess).Round(time.Hour)))
		}
		if st.LastError != "" {
			out = append(out, fmt.Sprintf("last backup to %s failed: %s", st.Name, st.LastError))
		}
		if st.LastCheck != nil && !st.LastCheck.OK() {
			out = append(out, fmt.Sprintf("last check of %s found %d problems", st.Name, len(st.LastCheck.Errors)+len(st.LastCheck.LocalMismatch)))
		}
	}
	return out
}

func (s *Service) alertStale(ctx context.Context) {
	// Once a day is enough.
	if time.Now().Hour() != 9 {
		return
	}
	for _, w := range s.Warnings() {
		if strings.Contains(w, "ago") || strings.Contains(w, "never") {
			s.notify(ctx, "PersonalDrive backup warning", w)
		}
	}
}

// notify sends an alert to ntfy and/or a webhook; failures are only logged.
func (s *Service) notify(ctx context.Context, title, body string) {
	n := s.Cfg.Backup.Notify
	if n.NtfyURL != "" {
		req, _ := http.NewRequestWithContext(ctx, "POST", n.NtfyURL, strings.NewReader(body))
		req.Header.Set("Title", title)
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "warning")
		s.send(req)
	}
	if n.WebhookURL != "" {
		payload, _ := json.Marshal(map[string]string{"title": title, "message": body, "server": s.Cfg.Server.Name})
		req, _ := http.NewRequestWithContext(ctx, "POST", n.WebhookURL, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		s.send(req)
	}
}

func (s *Service) send(req *http.Request) {
	resp, err := s.http.Do(req)
	if err != nil {
		s.Log.Warn("sending alert", "err", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.Log.Warn("alert endpoint returned an error", "status", resp.Status)
	}
}
