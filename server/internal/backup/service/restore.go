package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/engine"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

// Kit is the recovery kit: everything needed to rebuild the server on a new
// machine. It is printed/saved once, kept OFF the server (paper, password
// manager), and never stored in the backup itself.
type Kit struct {
	Server      string              `yaml:"server"`
	Created     time.Time           `yaml:"created"`
	Target      config.BackupTarget `yaml:"target"`
	RecoveryKey string              `yaml:"recovery_key,omitempty"`
}

const kitHeader = `# PersonalDrive RECOVERY KIT — keep this OFF the server.
# Print it, or store it in a password manager. Anyone with this file and
# access to the bucket can read your backup; without it, nobody can
# (including you). Restore on a new machine with:
#
#   scripts/bootstrap.sh --kit this-file.yaml --library /mnt/<disk>/library
#
# If the target uses S3 and has no keys below, export AWS_ACCESS_KEY_ID and
# AWS_SECRET_ACCESS_KEY first (ideally a read-only key made for recovery).
`

func WriteKit(w io.Writer, k Kit) error {
	data, err := yaml.Marshal(k)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, kitHeader); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func ReadKit(path string) (*Kit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var k Kit
	if err := yaml.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("recovery kit %s: %w", path, err)
	}
	if k.Target.Type == "" {
		return nil, errors.New("recovery kit has no target")
	}
	k.Target.ApplyDefaults()
	return &k, nil
}

// RestoreMetaOptions controls rebuilding the server's state directory.
type RestoreMetaOptions struct {
	Snapshot    string
	StateDir    string
	ConfigPath  string
	LibraryPath string
	Target      config.BackupTarget // written into the restored config
	Force       bool                // overwrite an existing database
	Progress    func(engine.RestoreProgress)
}

// RestoreMeta restores the database, secrets, TLS CA and config (all in hot
// storage, so this is fast even on Deep Archive), points the config at the
// new paths, and marks a restore as in progress so the server stays
// read-only until the library restore finishes.
func RestoreMeta(ctx context.Context, r *repo.Repo, o RestoreMetaOptions) error {
	dbPath := filepath.Join(o.StateDir, "pdrive.db")
	if _, err := os.Stat(dbPath); err == nil && !o.Force {
		return fmt.Errorf("%s already exists; refusing to overwrite (use --force)", dbPath)
	}
	tmp := filepath.Join(o.StateDir, "restore-meta")
	os.RemoveAll(tmp)
	opts := engine.RestoreOptions{
		Snapshot: o.Snapshot, Targets: map[string]string{SourceMeta: tmp}, Pricing: engine.DefaultPricing(),
		CacheDir: filepath.Join(o.StateDir, "restore-cache"), Progress: o.Progress,
	}
	plan, err := engine.PlanRestore(ctx, r, opts)
	if err != nil {
		return err
	}
	if plan.Files == 0 {
		return errors.New("snapshot has no server metadata")
	}
	if err := engine.Restore(ctx, r, plan, opts); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(dbPath + suffix)
	}
	moves := map[string]string{
		"pdrive.db":         dbPath,
		"secrets/token.key": filepath.Join(o.StateDir, "secrets", "token.key"),
		"tls/ca.pem":        filepath.Join(o.StateDir, "tls", "ca.pem"),
		"tls/ca.key":        filepath.Join(o.StateDir, "tls", "ca.key"),
	}
	for src, dst := range moves {
		if _, err := os.Stat(filepath.Join(tmp, src)); err != nil {
			continue
		}
		os.MkdirAll(filepath.Dir(dst), 0o700)
		if err := os.Rename(filepath.Join(tmp, src), dst); err != nil {
			return err
		}
		os.Chmod(dst, 0o600)
	}

	// Derived and in-flight state didn't come along: thumbnails are
	// regenerated, upload sessions and jobs start fresh.
	db, err := metadata.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening restored database: %w", err)
	}
	for _, q := range []string{
		`DELETE FROM processor_state WHERE processor = 'thumbnail'`,
		`DELETE FROM upload_sessions`,
		`DELETE FROM jobs`,
	} {
		if _, err := db.SQL().ExecContext(ctx, q); err != nil {
			db.Close()
			return err
		}
	}
	db.Close()

	cfg := config.Default()
	if data, err := os.ReadFile(filepath.Join(tmp, "config.yaml")); err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("restored config: %w", err)
		}
	}
	cfg.Paths.State, cfg.Paths.Library = o.StateDir, o.LibraryPath
	cfg.Backup.PassphraseFile = ""
	replaced := false
	for i := range cfg.Backup.Targets {
		if cfg.Backup.Targets[i].Name == o.Target.Name {
			cfg.Backup.Targets[i] = o.Target
			replaced = true
		}
	}
	if !replaced {
		cfg.Backup.Targets = append(cfg.Backup.Targets, o.Target)
	}
	// Local targets from the old machine probably aren't mounted here.
	var kept []config.BackupTarget
	for _, t := range cfg.Backup.Targets {
		if t.Type == "local" && t.Name != o.Target.Name {
			if _, err := os.Stat(t.Path); err != nil {
				continue
			}
		}
		kept = append(kept, t)
	}
	cfg.Backup.Targets = kept
	if err := cfg.Save(o.ConfigPath); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.StateDir, RestoreMarker), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// RestoreLibrary restores the library files and clears the restore marker
// when complete. The volume marker is always restored, so the server
// recognises the disk even if it was initialised before the restore.
func RestoreLibrary(ctx context.Context, r *repo.Repo, plan *engine.RestorePlan, opts engine.RestoreOptions, stateDir string) error {
	opts.AlwaysReplace = append(opts.AlwaysReplace, volume.MarkerName)
	if err := engine.Restore(ctx, r, plan, opts); err != nil {
		return err
	}
	if len(opts.Include) == 0 && stateDir != "" {
		os.Remove(filepath.Join(stateDir, RestoreMarker))
	}
	return nil
}

// RestoreInProgress reports whether the state dir has an unfinished restore.
func RestoreInProgress(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, RestoreMarker))
	return err == nil
}

// DescribeTarget is a short human description.
func DescribeTarget(t *config.BackupTarget) string {
	if t.Type == "local" {
		return "local disk " + t.Path
	}
	d := fmt.Sprintf("s3://%s/%s (%s)", t.Bucket, strings.Trim(t.Prefix, "/"), t.StorageClass)
	if t.Endpoint != "" {
		d += " via " + t.Endpoint
	}
	return d
}
