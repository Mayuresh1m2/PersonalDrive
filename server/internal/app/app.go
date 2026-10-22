// Package app wires the server components together. Both the daemon and the
// CLI build on it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/api"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/auth"
	backupsvc "github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/service"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/discovery"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/events"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/health"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/jobs"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/library"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/processors"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/tlsutil"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

// Version is set at build time with -ldflags "-X .../app.Version=...".
var Version = "dev"

const settingLibraryID = "volume.library.id"

type App struct {
	Cfg      *config.Config
	Log      *slog.Logger
	DB       *metadata.DB
	Store    *storage.LocalStore
	Guard    *volume.Guard
	Lib      *library.Service
	Auth     *auth.Service
	Bus      *events.Bus
	Notifier *events.Notifier
	Jobs     *jobs.Queue
	Runner   *processors.Runner
	Thumbs   *processors.Thumbnails
	Backup   *backupsvc.Service

	lock      *os.File
	setupCode string
	setupDone atomic.Bool
	wasOK     atomic.Bool
}

// Open builds the app. If exclusive is set it takes the state-dir lock, so
// only one process mutates the library at a time (the server, or a CLI
// command like import/reindex while the server is stopped).
func Open(cfg *config.Config, log *slog.Logger, exclusive bool) (*App, error) {
	if err := os.MkdirAll(cfg.Paths.State, 0o700); err != nil {
		return nil, err
	}
	a := &App{Cfg: cfg, Log: log}
	if exclusive {
		if err := a.acquireLock(); err != nil {
			return nil, err
		}
	}
	db, err := metadata.Open(cfg.StatePath("pdrive.db"))
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	a.DB = db

	expected, err := db.Setting(context.Background(), settingLibraryID)
	if err != nil {
		a.Close()
		return nil, err
	}
	a.Guard = volume.NewGuard(cfg.Paths.Library, volume.RoleLibrary, expected)
	a.checkRestoreMarker()
	a.Store = storage.NewLocalStore(cfg.Paths.Library, storage.LinkOff)
	a.onVolumeChange(context.Background())

	authSvc, err := auth.NewService(db, auth.Config{
		AccessTTL: cfg.Auth.AccessTTL.D(), RefreshTTL: cfg.Auth.RefreshTTL.D(), PairingTTL: cfg.Auth.PairingTTL.D(),
		MinPasswordChars: cfg.Auth.PasswordMinChars,
	}, cfg.StatePath("secrets"))
	if err != nil {
		a.Close()
		return nil, err
	}
	a.Auth = authSvc

	a.Bus = events.NewBus(log)
	seq, err := db.MaxSeq(context.Background())
	if err != nil {
		a.Close()
		return nil, err
	}
	a.Notifier = events.NewNotifier(seq)
	a.Bus.Subscribe("*", func(_ context.Context, e events.Event) error {
		a.Notifier.Advance(e.Seq)
		return nil
	})
	a.Lib = library.New(db, a.Store, a.Bus, a.Guard, library.Options{
		DedupScope: library.DedupScope(cfg.Dedup.Scope), TrashRetention: cfg.Trash.Retention.D(),
	}, log)

	a.Jobs = jobs.New(db.SQL(), log)
	ffmpeg := cfg.Processing.FFmpegPath
	if ffmpeg == "" {
		ffmpeg, _ = exec.LookPath("ffmpeg")
	}
	a.Thumbs = &processors.Thumbnails{CacheDir: cfg.StatePath("cache", "thumbs"), Sizes: cfg.Processing.ThumbnailSizes, FFmpeg: ffmpeg}
	a.Runner = processors.NewRunner(db, a.Jobs, a.Lib.AbsPath, log, a.Thumbs, &processors.MediaInfo{DB: db})
	a.Runner.Subscribe(a.Bus)
	a.Backup = backupsvc.New(cfg, db, log, "pdrive "+Version)
	return a, nil
}

// checkRestoreMarker keeps the library read-only while `pdrive restore` runs.
func (a *App) checkRestoreMarker() {
	if backupsvc.RestoreInProgress(a.Cfg.Paths.State) {
		a.Guard.SetBlocked("restore in progress — the library is read-only until `pdrive restore` finishes")
	} else {
		a.Guard.SetBlocked("")
	}
}

func (a *App) Close() {
	if a.DB != nil {
		a.DB.Close()
	}
	if a.lock != nil {
		syscall.Flock(int(a.lock.Fd()), syscall.LOCK_UN)
		a.lock.Close()
	}
}

var ErrLocked = errors.New("another pdrive process (probably the server) is using this state directory; stop it first")

func (a *App) acquireLock() error {
	f, err := os.OpenFile(a.Cfg.StatePath("pdrive.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return ErrLocked
	}
	a.lock = f
	return nil
}

// onVolumeChange reacts to the library disk appearing or disappearing.
func (a *App) onVolumeChange(ctx context.Context) {
	st := a.Guard.Status()
	if !st.OK {
		a.wasOK.Store(false)
		a.Log.Error("library volume unavailable; serving read-only", "path", st.Path, "problem", st.Problem)
		return
	}
	if a.wasOK.Swap(true) {
		return
	}
	// First time we see this disk: remember it so a different disk mounted at
	// the same path is refused later.
	if id, _ := a.DB.Setting(ctx, settingLibraryID); id == "" {
		if err := a.DB.SetSetting(ctx, settingLibraryID, st.ID); err != nil {
			a.Log.Error("saving library volume id", "err", err)
		}
		a.Guard.SetExpectedID(st.ID)
	}
	if err := a.Store.Prepare(); err != nil {
		a.Log.Error("preparing library", "err", err)
		return
	}
	mode, err := storage.ResolveLinkMode(a.Cfg.Paths.Library, a.Cfg.Dedup.OnDisk)
	if err != nil {
		a.Log.Error("on-disk dedup disabled", "err", err)
		mode = storage.LinkOff
	}
	a.Store.SetLinkMode(mode)
	a.Log.Info("library volume ready", "path", st.Path, "id", st.ID, "dedup", mode,
		"free_gb", st.FreeBytes>>30, "total_gb", st.TotalBytes>>30)
}

// SetupCode is required to create the first admin; "" once users exist.
func (a *App) SetupCode() string {
	if a.setupDone.Load() {
		return ""
	}
	if n, err := a.DB.CountUsers(context.Background()); err == nil && n > 0 {
		a.setupDone.Store(true)
		return ""
	}
	return a.setupCode
}

// HealthReport gathers volume, disk and job status.
func (a *App) HealthReport(ctx context.Context) health.Report {
	r := health.Report{At: time.Now().UTC(), Volumes: []volume.Status{a.Guard.Status()}}
	r.State = volume.Status{Path: a.Cfg.Paths.State, Role: "state", OK: true}
	r.State.FreeBytes, r.State.TotalBytes = volume.Usage(a.Cfg.Paths.State)
	r.ReadOnly = !r.Volumes[0].OK
	if r.ReadOnly {
		r.Warnings = append(r.Warnings, "library disk unavailable: "+r.Volumes[0].Problem)
	}
	for _, v := range append(r.Volumes, r.State) {
		if v.TotalBytes > 0 && int64(v.FreeBytes) < a.Cfg.Health.MinFreeBytes {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s (%s) is low on space: %d GiB free", v.Path, v.Role, v.FreeBytes>>30))
		}
	}
	for _, dev := range a.Cfg.Health.SmartDevices {
		s := health.Smart(ctx, dev)
		if s.Passed != nil && !*s.Passed {
			r.Warnings = append(r.Warnings, "SMART health check FAILED for "+dev+" — replace this disk")
		}
		r.Smart = append(r.Smart, s)
	}
	r.Jobs, _ = a.Jobs.Stats(ctx)
	if a.Backup != nil {
		r.Warnings = append(r.Warnings, a.Backup.Warnings()...)
	}
	r.OK = len(r.Warnings) == 0
	return r
}

// Serve runs the HTTP server and background tasks until ctx is cancelled.
func (a *App) Serve(ctx context.Context, extra func(*http.ServeMux, *api.Server)) error {
	if a.lock == nil {
		return errors.New("Serve requires the exclusive lock")
	}
	if err := a.DB.IntegrityCheck(ctx); err != nil {
		return err
	}
	if code := a.InitSetupCode(ctx); code != "" {
		a.Log.Warn("no users yet — open the app or run `pdrive user add <name> --admin`. Web/app setup code: " + code)
	}

	var tlsMgr *tlsutil.Manager
	if a.Cfg.Server.TLS {
		var err error
		if tlsMgr, err = tlsutil.Load(a.Cfg.StatePath("tls")); err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		a.Log.Info("TLS ready", "ca_sha256", tlsMgr.CAFingerprint())
	}
	srv := a.APIServer(tlsMgr, func(mux *http.ServeMux, s *api.Server) {
		a.registerBackupAPI(mux, s)
		if extra != nil {
			extra(mux, s)
		}
	})

	ln, err := net.Listen("tcp", a.Cfg.Server.Listen)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(a.Log.Handler(), slog.LevelWarn),
	}
	if tlsMgr != nil {
		httpSrv.TLSConfig = tlsMgr.TLSConfig()
	}

	if a.Cfg.Server.MDNS {
		port := ln.Addr().(*net.TCPAddr).Port
		if z, err := discovery.Advertise(a.Cfg.Server.Name, port, srv.CAFingerprint(), Version); err != nil {
			a.Log.Warn("mDNS advertisement failed; clients will need the address", "err", err)
		} else {
			defer z.Shutdown()
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.Jobs.Run(ctx, a.Cfg.Processing.Workers)
	go a.background(ctx, tlsMgr)
	if n, err := a.Runner.Backfill(ctx); err != nil {
		a.Log.Warn("backfilling processors", "err", err)
	} else if n > 0 {
		a.Log.Info("queued background processing for existing files", "jobs", n)
	}
	if err := a.Backup.Start(ctx); err != nil {
		return err
	}
	if len(a.Cfg.Backup.Targets) == 0 {
		a.Log.Warn("no backup targets configured — see docs/BACKUP.md")
	}

	errc := make(chan error, 1)
	go func() {
		a.Log.Info("listening", "addr", ln.Addr().String(), "tls", tlsMgr != nil, "urls", srv.URLs())
		if tlsMgr != nil {
			errc <- httpSrv.ServeTLS(ln, "", "")
		} else {
			errc <- httpSrv.Serve(ln)
		}
	}()
	select {
	case <-ctx.Done():
		shutdown, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		return httpSrv.Shutdown(shutdown)
	case err := <-errc:
		return err
	}
}

// APIServer builds the HTTP API on top of the app's components.
func (a *App) APIServer(tlsMgr *tlsutil.Manager, extra func(*http.ServeMux, *api.Server)) *api.Server {
	return api.New(api.Deps{
		Cfg: a.Cfg, DB: a.DB, Lib: a.Lib, Auth: a.Auth, Notifier: a.Notifier, Jobs: a.Jobs, Thumbs: a.Thumbs,
		TLS: tlsMgr, Health: a.HealthReport, Log: a.Log, Version: Version, SetupCode: a.SetupCode,
		Extra: extra,
	})
}

// InitSetupCode generates the first-run setup code if there are no users.
func (a *App) InitSetupCode(ctx context.Context) string {
	if n, err := a.DB.CountUsers(ctx); err == nil && n == 0 && a.setupCode == "" {
		a.setupCode = auth.RandomCode()
	}
	return a.SetupCode()
}

func (a *App) background(ctx context.Context, tlsMgr *tlsutil.Manager) {
	volTick := time.NewTicker(30 * time.Second)
	hourly := time.NewTicker(time.Hour)
	certTick := time.NewTicker(10 * time.Minute)
	defer volTick.Stop()
	defer hourly.Stop()
	defer certTick.Stop()
	a.maintenance(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-volTick.C:
			before := a.Guard.Status().OK
			a.checkRestoreMarker()
			a.Guard.Refresh()
			if a.Guard.Status().OK != before {
				a.onVolumeChange(ctx)
			}
		case <-hourly.C:
			a.maintenance(ctx)
		case <-certTick.C:
			if tlsMgr != nil {
				if changed, err := tlsMgr.Refresh(); err != nil {
					a.Log.Error("refreshing certificate", "err", err)
				} else if changed {
					a.Log.Info("server certificate re-issued for new network addresses")
				}
			}
		}
	}
}

// maintenance purges expired trash and abandoned uploads.
func (a *App) maintenance(ctx context.Context) {
	if n, err := a.Lib.PurgeExpiredTrash(ctx); err != nil && !errors.Is(err, volume.ErrUnavailable) {
		a.Log.Error("purging trash", "err", err)
	} else if n > 0 {
		a.Log.Info("purged expired trash", "entries", n)
	}
	ids, err := a.DB.ExpiredUploads(ctx, time.Now())
	if err == nil {
		for _, id := range ids {
			os.RemoveAll(a.Cfg.StatePath("staging", id))
			a.DB.DeleteUpload(ctx, id)
		}
	}
	if err := a.Lib.CleanupTemp(24 * time.Hour); err != nil && !errors.Is(err, volume.ErrUnavailable) {
		a.Log.Warn("cleaning temp files", "err", err)
	}
}

// ParsePort is a small helper for CLI output.
func ParsePort(listen string) int {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
