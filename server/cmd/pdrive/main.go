// Command pdrive is the PersonalDrive server and admin CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/app"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

var (
	configPath string
	logJSON    bool
)

func main() {
	root := &cobra.Command{
		Use:           "pdrive",
		Short:         "PersonalDrive home server and admin tool",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", config.DefaultPath(), "config file")
	root.PersistentFlags().BoolVar(&logJSON, "log-json", false, "log as JSON (for journald/log shippers)")

	root.AddCommand(serveCmd(), initCmd(), versionCmd(), volumeCmd(), userCmd(), spaceCmd(), pairCmd(),
		reindexCmd(), importCmd(), dupesCmd(), backupCmd(), restoreCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// logger returns the server logger; admin commands only show warnings.
func logger(level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if logJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("%w\n(config file: %s — create one with `pdrive init`)", err, configPath)
	}
	return cfg, nil
}

// openApp opens the app; exclusive commands fail if the server is running.
func openApp(exclusive bool) (*app.App, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return app.Open(cfg, logger(slog.LevelWarn), exclusive)
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			a, err := app.Open(cfg, logger(slog.LevelInfo), true)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx, cancel := signalContext()
			defer cancel()
			return a.Serve(ctx, nil)
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(cmd *cobra.Command, args []string) { fmt.Println(app.Version) },
	}
}

func initCmd() *cobra.Command {
	var library, state, listen string
	var noTLS bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a config file and initialise the library disk",
		Long: `Creates the config file (if missing), the state directory, and the
.pdrive-volume marker on the library disk. If the disk isn't plugged in yet,
the config is still written and you can run 'pdrive volume init' later.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Read(configPath)
			if err != nil {
				return err
			}
			if library != "" {
				cfg.Paths.Library, _ = filepath.Abs(library)
			}
			if state != "" {
				cfg.Paths.State, _ = filepath.Abs(state)
			}
			if listen != "" {
				cfg.Server.Listen = listen
			}
			if noTLS {
				cfg.Server.TLS = false
			}
			if cfg.Paths.Library == "" {
				return errors.New("--library is required (where your primary library disk is mounted)")
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			if err := cfg.Save(configPath); err != nil {
				return err
			}
			fmt.Println("config written:", configPath)
			if err := os.MkdirAll(cfg.Paths.State, 0o700); err != nil {
				return err
			}
			fmt.Println("state directory:", cfg.Paths.State)

			m, err := volume.Init(cfg.Paths.Library, volume.RoleLibrary)
			if err != nil {
				fmt.Printf("\nlibrary disk not initialised yet: %v\n", err)
				fmt.Printf("plug it in / mount it, then run: pdrive volume init --role library %s\n", cfg.Paths.Library)
			} else {
				fmt.Printf("library volume ready: %s (id %s)\n", cfg.Paths.Library, m.ID)
			}
			fmt.Println("\nnext steps:")
			fmt.Println("  pdrive user add <name> --admin     # create your account")
			fmt.Println("  pdrive serve                       # start the server")
			return nil
		},
	}
	cmd.Flags().StringVar(&library, "library", "", "primary library directory (external disk A mount point)")
	cmd.Flags().StringVar(&state, "state", "", "state directory for DB, cache, TLS (default ~/.local/share/pdrive)")
	cmd.Flags().StringVar(&listen, "listen", "", "listen address (default :8443)")
	cmd.Flags().BoolVar(&noTLS, "no-tls", false, "serve plain HTTP (development only)")
	return cmd
}

func volumeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "volume", Short: "Manage disk markers"}
	var role string
	initC := &cobra.Command{
		Use:   "init PATH",
		Short: "Write a .pdrive-volume marker to a mounted disk",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := volume.Init(args[0], role)
			if err != nil {
				return err
			}
			fmt.Printf("%s volume ready at %s (id %s)\n", m.Role, args[0], m.ID)
			return nil
		},
	}
	initC.Flags().StringVar(&role, "role", volume.RoleLibrary, "library | backup")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether configured disks are present",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			s := volume.Check(cfg.Paths.Library, volume.RoleLibrary, "")
			state := "OK"
			if !s.OK {
				state = "MISSING — " + s.Problem
			}
			fmt.Printf("library  %-40s %s  (%d GiB free of %d GiB)\n", s.Path, state, s.FreeBytes>>30, s.TotalBytes>>30)
			return nil
		},
	}
	cmd.AddCommand(initC, status)
	return cmd
}
