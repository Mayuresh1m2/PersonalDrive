// Command pdrive-sync keeps local folders in two-way sync with a
// PersonalDrive home server. It runs on Linux, macOS and Windows.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"

	"github.com/Mayuresh1m2/PersonalDrive/desktop_app/internal/client"
	"github.com/Mayuresh1m2/PersonalDrive/desktop_app/internal/syncer"
)

type Config struct {
	Server string        `json:"server"`
	CAPEM  string        `json:"ca_pem,omitempty"`
	Tokens client.Tokens `json:"tokens"`
	Device string        `json:"device"`
	Pairs  []syncer.Pair `json:"pairs"`
}

var cfgDir string

func configPath() string { return filepath.Join(cfgDir, "config.json") }

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, json.Unmarshal(data, &c)
}

var saveMu sync.Mutex

func (c *Config) save() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}

func (c *Config) client() (*client.Client, error) {
	if c.Server == "" || c.Tokens.RefreshToken == "" {
		return nil, errors.New("not connected to a server: run `pdrive-sync pair` first")
	}
	cl, err := client.New(c.Server, []byte(c.CAPEM), c.Tokens)
	if err != nil {
		return nil, err
	}
	cl.SaveTokens = func(t client.Tokens) error {
		c.Tokens = t
		return c.save()
	}
	return cl, nil
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, nil)) }

func main() {
	def, _ := os.UserConfigDir()
	root := &cobra.Command{Use: "pdrive-sync", Short: "Two-way folder sync with your PersonalDrive server", SilenceUsage: true}
	root.PersistentFlags().StringVar(&cfgDir, "config-dir", filepath.Join(def, "pdrive-sync"), "where settings and sync state live")
	root.AddCommand(pairCmd(), loginCmd(), addCmd(), removeCmd(), statusCmd(), onceCmd(), runCmd(), serviceCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s (%s)", h, runtime.GOOS)
}

func pairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pair PAYLOAD",
		Short: "Connect using the pairing code from the web app (Devices → Show pairing code) or `pdrive pair`",
		Long:  "PAYLOAD is the JSON text inside the pairing QR code (`pdrive pair <user>` prints it). Use - to read it from stdin.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := args[0]
			if text == "-" {
				b, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				text = b
			}
			var p struct {
				V        int      `json:"v"`
				URLs     []string `json:"urls"`
				CASHA256 string   `json:"ca_sha256"`
				Token    string   `json:"token"`
			}
			if err := json.Unmarshal([]byte(text), &p); err != nil || p.V != 1 || p.Token == "" {
				return errors.New("that isn't a PersonalDrive pairing code")
			}
			ctx := context.Background()
			c, _ := loadConfig()
			var lastErr error
			for _, u := range p.URLs {
				var ca []byte
				if strings.HasPrefix(u, "https://") && p.CASHA256 != "" {
					var err error
					if ca, err = client.FetchCA(ctx, u, p.CASHA256); err != nil {
						lastErr = err
						continue
					}
				}
				cl, err := client.New(u, ca, client.Tokens{})
				if err != nil {
					return err
				}
				if err := cl.Pair(ctx, p.Token, hostname()); err != nil {
					lastErr = err
					if client.IsStatus(err, 401) {
						return errors.New("pairing code expired or already used; make a new one")
					}
					continue
				}
				c.Server, c.CAPEM, c.Tokens, c.Device = u, string(ca), cl.Tokens(), hostname()
				if err := c.save(); err != nil {
					return err
				}
				fmt.Printf("connected to %s\nnext: pdrive-sync add ~/PersonalDrive\n", u)
				return nil
			}
			return fmt.Errorf("couldn't reach the server at any of %v: %v", p.URLs, lastErr)
		},
	}
}

func loginCmd() *cobra.Command {
	var server, user, caFile string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Connect with a username and password (password read from stdin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var ca []byte
			if caFile != "" {
				var err error
				if ca, err = os.ReadFile(caFile); err != nil {
					return err
				}
			}
			cl, err := client.New(strings.TrimRight(server, "/"), ca, client.Tokens{})
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stderr, "Password: ")
			pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if err := cl.Login(context.Background(), user, strings.TrimRight(pw, "\r\n"), hostname()); err != nil {
				return err
			}
			c, _ := loadConfig()
			c.Server, c.CAPEM, c.Tokens, c.Device = cl.BaseURL, string(ca), cl.Tokens(), hostname()
			return c.save()
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "server URL, e.g. https://home.local:8443")
	cmd.Flags().StringVar(&user, "user", "", "username")
	cmd.Flags().StringVar(&caFile, "ca", "", "server CA certificate (download from /api/v1/ca.pem)")
	cmd.MarkFlagRequired("server")
	cmd.MarkFlagRequired("user")
	return cmd
}

func addCmd() *cobra.Command {
	var remote, mode string
	var space int64
	var syncDeletes bool
	cmd := &cobra.Command{
		Use:   "add LOCAL_FOLDER",
		Short: "Sync a local folder with a server folder (default: a server folder with the same name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			local, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			c, err := loadConfig()
			if err != nil {
				return err
			}
			if remote == "" {
				remote = filepath.Base(local)
			}
			remote = strings.Trim(filepath.ToSlash(remote), "/")
			for _, p := range c.Pairs {
				if p.Local == local || strings.HasPrefix(local, p.Local+string(filepath.Separator)) || strings.HasPrefix(p.Local, local+string(filepath.Separator)) {
					return fmt.Errorf("%s overlaps an existing synced folder (%s)", local, p.Local)
				}
			}
			if mode != syncer.ModeUploadOnly && mode != syncer.ModeTwoWay {
				return fmt.Errorf("--mode must be %s or %s", syncer.ModeUploadOnly, syncer.ModeTwoWay)
			}
			if syncDeletes && mode != syncer.ModeTwoWay {
				return errors.New("--sync-deletes only applies to --mode two-way")
			}
			c.Pairs = append(c.Pairs, syncer.Pair{Local: local, Remote: remote, SpaceID: space, Mode: mode, SyncDeletes: syncDeletes})
			if err := os.MkdirAll(local, 0o755); err != nil {
				return err
			}
			fmt.Printf("%s → server:/%s (%s%s)\n", local, remote, mode, map[bool]string{true: ", deletions synced"}[syncDeletes])
			return c.save()
		},
	}
	cmd.Flags().StringVar(&mode, "mode", syncer.ModeUploadOnly, "upload-only (never changes or deletes anything) | two-way")
	cmd.Flags().BoolVar(&syncDeletes, "sync-deletes", false, "two-way only: also propagate deletions (removed local files go to the sync trash)")
	cmd.Flags().StringVar(&remote, "remote", "", "server folder (\"/\" for the whole library)")
	cmd.Flags().Int64Var(&space, "space", 0, "shared space id (default: your own library)")
	return cmd
}

func removeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove LOCAL_FOLDER",
		Short: "Stop syncing a folder (files stay where they are)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			local, _ := filepath.Abs(args[0])
			c, err := loadConfig()
			if err != nil {
				return err
			}
			var kept []syncer.Pair
			for _, p := range c.Pairs {
				if p.Local != local {
					kept = append(kept, p)
				}
			}
			if len(kept) == len(c.Pairs) {
				return fmt.Errorf("%s is not synced", local)
			}
			c.Pairs = kept
			return c.save()
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the server and synced folders",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			if c.Server == "" {
				fmt.Println("not connected (run `pdrive-sync pair`)")
				return nil
			}
			fmt.Printf("server: %s (as %s)\n", c.Server, c.Device)
			for _, p := range c.Pairs {
				mode := p.Mode
				if mode == "" {
					mode = syncer.ModeUploadOnly
				}
				if p.SyncDeletes {
					mode += ", deletions synced"
				}
				fmt.Printf("  %s ⇄ server:/%s [%s]", p.Local, p.Remote, mode)
				if p.SpaceID != 0 {
					fmt.Printf(" (space %d)", p.SpaceID)
				}
				fmt.Println()
			}
			fmt.Printf("files removed by sync are kept in %s\n", filepath.Join(cfgDir, "state", "trash"))
			return nil
		},
	}
}

func syncers(c *Config) ([]*syncer.Syncer, error) {
	cl, err := c.client()
	if err != nil {
		return nil, err
	}
	if len(c.Pairs) == 0 {
		return nil, errors.New("no folders to sync: run `pdrive-sync add <folder>`")
	}
	var out []*syncer.Syncer
	for _, p := range c.Pairs {
		out = append(out, &syncer.Syncer{C: cl, Pair: p, StateDir: filepath.Join(cfgDir, "state"), Log: logger()})
	}
	return out, nil
}

func onceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "once",
		Short: "Sync every folder once and exit",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			ss, err := syncers(c)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			var failed bool
			for _, s := range ss {
				r, err := s.SyncOnce(ctx)
				if err != nil {
					fmt.Printf("%s: %v\n", s.Pair.Local, err)
					failed = true
					continue
				}
				fmt.Printf("%s: %s\n", s.Pair.Local, r)
				for _, e := range r.Errors {
					fmt.Println("  error:", e)
				}
			}
			if failed {
				return errors.New("some folders failed to sync")
			}
			return nil
		},
	}
}

func runCmd() *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Keep folders in sync (watches local changes and the server's change feed)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadConfig()
			if err != nil {
				return err
			}
			ss, err := syncers(c)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			trigger := make(chan struct{}, 1)
			kick := func() {
				select {
				case trigger <- struct{}{}:
				default:
				}
			}
			go watchLocal(ctx, ss, kick)
			go watchRemote(ctx, ss[0].C, kick)
			log := logger()
			log.Info("syncing", "folders", len(ss), "server", c.Server)
			tick := time.NewTicker(interval)
			defer tick.Stop()
			backoff := time.Second
			for {
				hadErr := false
				for _, s := range ss {
					r, err := s.SyncOnce(ctx)
					switch {
					case ctx.Err() != nil:
						return nil
					case err != nil:
						hadErr = true
						log.Warn("sync failed; will retry", "folder", s.Pair.Local, "err", err)
					case r.Changed() || len(r.Errors) > 0:
						log.Info("synced", "folder", s.Pair.Local, "result", r.String())
						for _, e := range r.Errors {
							log.Warn("sync error", "folder", s.Pair.Local, "err", e)
						}
						hadErr = hadErr || r.Skipped > 0
					}
				}
				wait := interval
				if hadErr {
					wait, backoff = backoff, min(backoff*2, 5*time.Minute)
				} else {
					backoff = time.Second
				}
				select {
				case <-ctx.Done():
					return nil
				case <-trigger:
					// Let bursts of changes (copying a folder) finish first.
					time.Sleep(2 * time.Second)
				case <-time.After(wait):
				case <-tick.C:
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "full rescan interval (changes are picked up immediately anyway)")
	return cmd
}

// watchLocal triggers a sync on filesystem events in any synced folder.
func watchLocal(ctx context.Context, ss []*syncer.Syncer, kick func()) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		logger().Warn("file watching unavailable; relying on periodic scans", "err", err)
		return
	}
	defer w.Close()
	add := func(root string) {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				if syncer.Ignored(d.Name()) && p != root {
					return filepath.SkipDir
				}
				w.Add(p)
			}
			return nil
		})
	}
	for _, s := range ss {
		add(s.Pair.Local)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if syncer.Ignored(filepath.Base(ev.Name)) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					add(ev.Name)
				}
			}
			kick()
		case <-w.Errors:
		}
	}
}

// watchRemote long-polls the change feed only to wake the sync loop.
func watchRemote(ctx context.Context, cl *client.Client, kick func()) {
	cursor, err := cl.Cursor(ctx)
	for err != nil && ctx.Err() == nil {
		time.Sleep(30 * time.Second)
		cursor, err = cl.Cursor(ctx)
	}
	for ctx.Err() == nil {
		changes, next, _, err := cl.Changes(ctx, cursor, 55)
		if err != nil {
			time.Sleep(15 * time.Second)
			continue
		}
		if len(changes) > 0 {
			cursor = next
			kick()
		}
	}
}

func serviceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "service",
		Short: "Print instructions to run pdrive-sync in the background at login",
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, _ := os.Executable()
			switch runtime.GOOS {
			case "linux":
				fmt.Printf(`# ~/.config/systemd/user/pdrive-sync.service
[Unit]
Description=PersonalDrive folder sync
After=network-online.target

[Service]
ExecStart=%s run
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target

# then: systemctl --user daemon-reload && systemctl --user enable --now pdrive-sync
`, exe)
			case "darwin":
				fmt.Printf(`<!-- ~/Library/LaunchAgents/com.personaldrive.sync.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.personaldrive.sync</string>
  <key>ProgramArguments</key><array><string>%s</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
<!-- then: launchctl load ~/Library/LaunchAgents/com.personaldrive.sync.plist -->
`, exe)
			case "windows":
				fmt.Printf("schtasks /Create /SC ONLOGON /TN PersonalDriveSync /TR \"\\\"%s\\\" run\"\n", exe)
			}
			return nil
		},
	}
}
