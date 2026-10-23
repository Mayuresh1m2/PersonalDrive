// Package config loads the PersonalDrive YAML configuration.
//
// Every option has a default so a minimal config only needs paths.library.
// A handful of PDRIVE_* environment variables override file values.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Paths      PathsConfig      `yaml:"paths"`
	Dedup      DedupConfig      `yaml:"dedup"`
	Trash      TrashConfig      `yaml:"trash"`
	Uploads    UploadsConfig    `yaml:"uploads"`
	Auth       AuthConfig       `yaml:"auth"`
	Processing ProcessingConfig `yaml:"processing"`
	Health     HealthConfig     `yaml:"health"`
	Backup     BackupConfig     `yaml:"backup"`
	Safety     SafetyConfig     `yaml:"safety"`

	// file is the path the config was loaded from, if any.
	file string
}

type ServerConfig struct {
	// Name is shown to clients and advertised over mDNS. Defaults to the hostname.
	Name string `yaml:"name"`
	// Listen is the address the API binds to. Bind to a LAN address in production.
	Listen string `yaml:"listen"`
	// TLS serves HTTPS with a locally generated CA. Disable only for development.
	TLS bool `yaml:"tls"`
	// MDNS advertises the server as _pdrive._tcp on the local network.
	MDNS bool `yaml:"mdns"`
	// PublicURL overrides the URL put in pairing QR codes (e.g. https://laptop.local:8443).
	PublicURL string `yaml:"public_url"`
	// Metrics exposes Prometheus metrics at /metrics.
	Metrics bool `yaml:"metrics"`
}

type PathsConfig struct {
	// State holds the database, secrets, TLS material, thumbnail cache and upload staging.
	// Keep it on fast local storage (the laptop SSD).
	State string `yaml:"state"`
	// Library is the primary file library (external disk A). It must contain a
	// .pdrive-volume marker created by `pdrive volume init`.
	Library string `yaml:"library"`
}

type DedupConfig struct {
	// OnDisk: auto | reflink | hardlink | off.
	OnDisk string `yaml:"on_disk"`
	// Scope: household | user. Controls whether "already have it" answers consider
	// every user's files or only files the caller can see.
	Scope string `yaml:"scope"`
}

type TrashConfig struct {
	Retention Duration `yaml:"retention"`
}

type UploadsConfig struct {
	PartSize int64    `yaml:"part_size"`
	Expiry   Duration `yaml:"expiry"`
}

type AuthConfig struct {
	AccessTTL        Duration `yaml:"access_ttl"`
	RefreshTTL       Duration `yaml:"refresh_ttl"`
	PairingTTL       Duration `yaml:"pairing_ttl"`
	LoginRatePerMin  int      `yaml:"login_rate_per_minute"`
	PasswordMinChars int      `yaml:"password_min_chars"`
}

type ProcessingConfig struct {
	Workers        int    `yaml:"workers"`
	ThumbnailSizes []int  `yaml:"thumbnail_sizes"`
	FFmpegPath     string `yaml:"ffmpeg_path"`
}

type HealthConfig struct {
	// SmartDevices lists block devices to query with smartctl (e.g. /dev/sdb).
	SmartDevices []string `yaml:"smart_devices"`
	// MinFreeBytes raises a warning when a volume has less free space than this.
	MinFreeBytes int64 `yaml:"min_free_bytes"`
}

// SafetyConfig gates every action that permanently deletes data. All are
// off by default: turn each on only after the previous rollout stage has
// proven the copies and backups are good (see docs/ROLLOUT.md).
type SafetyConfig struct {
	// PurgeTrash empties trash entries older than trash.retention.
	// Off: the trash is kept forever.
	PurgeTrash bool `yaml:"purge_trash"`
	// PermanentDelete allows "Delete forever" from the trash.
	PermanentDelete bool `yaml:"permanent_delete"`
	// PruneBackups lets retention policies delete old snapshots and data
	// from backup targets. Off: backups only ever grow.
	PruneBackups bool `yaml:"prune_backups"`
	// FreeUpSpace lets phones delete local photos that are backed up.
	FreeUpSpace bool `yaml:"free_up_space"`
}

// BackupConfig configures backup targets (Phase 2).
type BackupConfig struct {
	// PassphraseFile holds the repository passphrase so scheduled backups can
	// run unattended (0600, on the laptop SSD). Default: <state>/secrets/backup-passphrase.
	PassphraseFile string `yaml:"passphrase_file"`
	// PackSize is the target size of pack objects. Larger packs mean fewer
	// (expensive) cold-tier PUT requests.
	PackSize int64          `yaml:"pack_size"`
	Targets  []BackupTarget `yaml:"targets"`
	Notify   NotifyConfig   `yaml:"notify"`
}

type BackupTarget struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // local | s3
	// local
	Path string `yaml:"path,omitempty"`
	// s3 and S3-compatible
	Bucket          string `yaml:"bucket,omitempty"`
	Prefix          string `yaml:"prefix,omitempty"`
	Region          string `yaml:"region,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty"`
	SecretAccessKey string `yaml:"secret_access_key,omitempty"`
	PathStyle       bool   `yaml:"path_style,omitempty"`
	// StorageClass for file content (cold) packs, e.g. DEEP_ARCHIVE,
	// GLACIER_IR, STANDARD_IA, STANDARD. Metadata always uses STANDARD.
	StorageClass   string    `yaml:"storage_class,omitempty"`
	BandwidthLimit int64     `yaml:"bandwidth_limit,omitempty"` // bytes/second, 0 = unlimited
	Schedule       string    `yaml:"schedule,omitempty"`        // cron, e.g. "0 2 * * *"
	PruneSchedule  string    `yaml:"prune_schedule,omitempty"`
	CheckSchedule  string    `yaml:"check_schedule,omitempty"`
	Retention      Retention `yaml:"retention,omitempty"`
}

type Retention struct {
	Last    int `yaml:"last,omitempty"`
	Hourly  int `yaml:"hourly,omitempty"`
	Daily   int `yaml:"daily,omitempty"`
	Weekly  int `yaml:"weekly,omitempty"`
	Monthly int `yaml:"monthly,omitempty"`
	Yearly  int `yaml:"yearly,omitempty"`
}

type NotifyConfig struct {
	// NtfyURL, e.g. https://ntfy.sh/my-secret-topic, gets a push on failures.
	NtfyURL    string `yaml:"ntfy_url,omitempty"`
	WebhookURL string `yaml:"webhook_url,omitempty"` // JSON POST
	// StaleAfter alerts when a target hasn't succeeded for this long.
	StaleAfter Duration `yaml:"stale_after,omitempty"`
}

// Target returns the backup target with the given name.
func (c *Config) Target(name string) (*BackupTarget, error) {
	for i := range c.Backup.Targets {
		if c.Backup.Targets[i].Name == name {
			return &c.Backup.Targets[i], nil
		}
	}
	return nil, fmt.Errorf("no backup target named %q in config", name)
}

// ApplyDefaults fills in per-type defaults.
func (t *BackupTarget) ApplyDefaults() {
	switch t.Type {
	case "local":
		if t.Schedule == "" {
			t.Schedule = "0 * * * *" // hourly: local backups are free
		}
		if t.Retention == (Retention{}) {
			t.Retention = Retention{Hourly: 24, Daily: 14, Weekly: 8, Monthly: 24, Yearly: 10}
		}
		if t.CheckSchedule == "" {
			t.CheckSchedule = "30 4 * * 0"
		}
	case "s3":
		if t.StorageClass == "" {
			t.StorageClass = "DEEP_ARCHIVE"
		}
		if t.Schedule == "" {
			t.Schedule = "0 2 * * *" // nightly
		}
		if t.Retention == (Retention{}) {
			// Deep Archive bills 180 days minimum, so keeping months of
			// snapshots costs almost nothing extra.
			t.Retention = Retention{Daily: 7, Weekly: 8, Monthly: 12, Yearly: 5}
		}
		if t.CheckSchedule == "" {
			t.CheckSchedule = "0 5 1 * *"
		}
	}
	if t.PruneSchedule == "" {
		t.PruneSchedule = "0 4 * * 0" // weekly
	}
}

// Duration is a time.Duration that unmarshals from strings like "720h" or "15m".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Default returns the configuration used when a key is not set.
func Default() *Config {
	host, _ := os.Hostname()
	return &Config{
		Server: ServerConfig{
			Name:    host,
			Listen:  ":8443",
			TLS:     true,
			MDNS:    true,
			Metrics: true,
		},
		Paths: PathsConfig{
			State: defaultStateDir(),
		},
		Dedup: DedupConfig{OnDisk: "auto", Scope: "household"},
		Trash: TrashConfig{Retention: Duration(30 * 24 * time.Hour)},
		Uploads: UploadsConfig{
			PartSize: 8 << 20,
			Expiry:   Duration(24 * time.Hour),
		},
		Auth: AuthConfig{
			AccessTTL:        Duration(15 * time.Minute),
			RefreshTTL:       Duration(90 * 24 * time.Hour),
			PairingTTL:       Duration(10 * time.Minute),
			LoginRatePerMin:  10,
			PasswordMinChars: 10,
		},
		Processing: ProcessingConfig{
			Workers:        2,
			ThumbnailSizes: []int{256, 1024},
		},
		Health: HealthConfig{MinFreeBytes: 10 << 30},
		Backup: BackupConfig{
			PackSize: 32 << 20,
			Notify:   NotifyConfig{StaleAfter: Duration(48 * time.Hour)},
		},
	}
}

func defaultStateDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "pdrive")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/var/lib/pdrive"
	}
	return filepath.Join(home, ".local", "share", "pdrive")
}

// DefaultPath returns the config file location: $PDRIVE_CONFIG, else
// ~/.config/pdrive/config.yaml, else /etc/pdrive/config.yaml.
func DefaultPath() string {
	if p := os.Getenv("PDRIVE_CONFIG"); p != "" {
		return p
	}
	if dir, err := os.UserConfigDir(); err == nil {
		p := filepath.Join(dir, "pdrive", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat("/etc/pdrive/config.yaml"); err == nil {
		return "/etc/pdrive/config.yaml"
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "pdrive", "config.yaml")
	}
	return "/etc/pdrive/config.yaml"
}

// Load reads the config file at path (missing file = defaults), applies
// environment overrides and validates the result.
func Load(path string) (*Config, error) {
	cfg, err := Read(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Read is Load without validation (for tools that fill in missing values).
func Read(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		cfg.file = path
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	cfg.applyEnv()
	cfg.Paths.State = expandHome(cfg.Paths.State)
	cfg.Paths.Library = expandHome(cfg.Paths.Library)
	cfg.Backup.PassphraseFile = expandHome(cfg.Backup.PassphraseFile)
	return cfg, nil
}

// PassphrasePath is where the backup passphrase is kept.
func (c *Config) PassphrasePath() string {
	if c.Backup.PassphraseFile != "" {
		return c.Backup.PassphraseFile
	}
	return c.StatePath("secrets", "backup-passphrase")
}

// File returns the path the config was loaded from ("" if defaults only).
func (c *Config) File() string { return c.file }

func (c *Config) applyEnv() {
	for env, dst := range map[string]*string{
		"PDRIVE_STATE":   &c.Paths.State,
		"PDRIVE_LIBRARY": &c.Paths.Library,
		"PDRIVE_LISTEN":  &c.Server.Listen,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("PDRIVE_TLS"); v != "" {
		c.Server.TLS = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("PDRIVE_MDNS"); v != "" {
		c.Server.MDNS = v == "1" || strings.EqualFold(v, "true")
	}
}

func (c *Config) Validate() error {
	var errs []error
	if c.Paths.State == "" {
		errs = append(errs, errors.New("paths.state is required"))
	}
	if c.Paths.Library == "" {
		errs = append(errs, errors.New("paths.library is required (run `pdrive init`)"))
	}
	switch c.Dedup.OnDisk {
	case "auto", "reflink", "hardlink", "off":
	default:
		errs = append(errs, fmt.Errorf("dedup.on_disk must be auto|reflink|hardlink|off, got %q", c.Dedup.OnDisk))
	}
	switch c.Dedup.Scope {
	case "household", "user":
	default:
		errs = append(errs, fmt.Errorf("dedup.scope must be household|user, got %q", c.Dedup.Scope))
	}
	if c.Uploads.PartSize < 1<<20 || c.Uploads.PartSize > 256<<20 {
		errs = append(errs, errors.New("uploads.part_size must be between 1 MiB and 256 MiB"))
	}
	if c.Processing.Workers < 1 {
		errs = append(errs, errors.New("processing.workers must be >= 1"))
	}
	names := map[string]bool{}
	for i := range c.Backup.Targets {
		t := &c.Backup.Targets[i]
		switch {
		case t.Name == "" || strings.ContainsAny(t.Name, " /"):
			errs = append(errs, fmt.Errorf("backup target %d needs a name without spaces or slashes", i+1))
		case names[t.Name]:
			errs = append(errs, fmt.Errorf("duplicate backup target %q", t.Name))
		case t.Type == "local" && t.Path == "":
			errs = append(errs, fmt.Errorf("backup target %q: path is required", t.Name))
		case t.Type == "s3" && t.Bucket == "":
			errs = append(errs, fmt.Errorf("backup target %q: bucket is required", t.Name))
		case t.Type != "local" && t.Type != "s3":
			errs = append(errs, fmt.Errorf("backup target %q: type must be local or s3", t.Name))
		}
		names[t.Name] = true
		t.Path = expandHome(t.Path)
		t.ApplyDefaults()
	}
	return errors.Join(errs...)
}

// Save writes the config as YAML with 0600 permissions.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// StatePath joins elements under the state directory.
func (c *Config) StatePath(elem ...string) string {
	return filepath.Join(append([]string{c.Paths.State}, elem...)...)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
