package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/crypto"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/engine"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/objectstore"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/repo"
	backupsvc "github.com/Mayuresh1m2/PersonalDrive/server/internal/backup/service"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/metadata"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/volume"
)

// openBackupService opens just what backups need (no exclusive lock: backups
// run alongside the server and coordinate through their own lock).
func openBackupService() (*backupsvc.Service, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	db, err := metadata.Open(cfg.StatePath("pdrive.db"))
	if err != nil {
		return nil, nil, err
	}
	svc := backupsvc.New(cfg, db, logger(slog.LevelWarn), "pdrive")
	return svc, func() { db.Close() }, nil
}

func targetNames(cfg *config.Config, args []string) []string {
	if len(args) > 0 {
		return args
	}
	var out []string
	for _, t := range cfg.Backup.Targets {
		out = append(out, t.Name)
	}
	return out
}

func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the library to disk B and S3",
		Long: `Backups are incremental, deduplicated, compressed and encrypted snapshots of
the library plus server metadata. Configure targets under backup.targets in the
config file (see docs/BACKUP.md), then run 'pdrive backup init <target>'.`,
	}
	cmd.AddCommand(backupInitCmd(), backupRunCmd(), backupStatusCmd(), backupSnapshotsCmd(),
		backupCheckCmd(), backupPruneCmd(), backupUnlockCmd(), backupKitCmd(), backupNewRecoveryKeyCmd(), backupPasswdCmd())
	return cmd
}

func readSecret(prompt string, confirm bool) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, prompt)
	a, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil || !confirm {
		return string(a), err
	}
	fmt.Fprint(os.Stderr, "Repeat: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passphrases don't match")
	}
	return string(a), nil
}

func backupInitCmd() *cobra.Command {
	var createBucket bool
	var kitOut string
	cmd := &cobra.Command{
		Use:   "init TARGET",
		Short: "Create the encrypted repository on a target and print the recovery kit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := context.Background()
			t, err := svc.Cfg.Target(args[0])
			if err != nil {
				return err
			}
			pass, err := svc.Passphrase()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Choose a backup passphrase (12+ characters). It's stored on this machine for")
				fmt.Fprintln(os.Stderr, "scheduled backups; the recovery key printed below is your way in if this machine is lost.")
				if pass, err = readSecret("Backup passphrase: ", true); err != nil {
					return err
				}
				if len(pass) < 12 {
					return errors.New("passphrase must be at least 12 characters")
				}
				if err := backupsvc.SavePassphrase(svc.Cfg.PassphrasePath(), pass); err != nil {
					return err
				}
			}
			store, err := backupsvc.OpenStore(ctx, t)
			if err != nil {
				return err
			}
			if s3, ok := store.(*objectstore.S3); ok {
				if err := s3.EnsureBucket(ctx, createBucket); err != nil {
					return err
				}
			}
			rk, err := repo.Init(ctx, store, repo.InitOptions{Passphrase: pass, PackSize: svc.Cfg.Backup.PackSize, Classes: backupsvc.Classes(t)})
			if err != nil {
				return err
			}
			kit := backupsvc.Kit{Server: svc.Cfg.Server.Name, Created: time.Now().UTC(), Target: *t, RecoveryKey: rk}
			fmt.Printf("repository created on %s\n\n", backupsvc.DescribeTarget(t))
			printKit(kit, kitOut)
			return nil
		},
	}
	cmd.Flags().BoolVar(&createBucket, "create-bucket", false, "create the S3 bucket if it doesn't exist")
	cmd.Flags().StringVar(&kitOut, "kit-out", "", "also write the recovery kit to this file (move it off the server!)")
	return cmd
}

func printKit(kit backupsvc.Kit, out string) {
	fmt.Println("================ RECOVERY KIT — SAVE THIS NOW ================")
	backupsvc.WriteKit(os.Stdout, kit)
	fmt.Println("==============================================================")
	if kit.RecoveryKey != "" {
		fmt.Println("The recovery key is shown ONLY now and is not stored on this machine.")
		fmt.Println("Without it (or your passphrase), the backup can't be decrypted by anyone.")
	}
	if out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "could not write kit:", err)
			return
		}
		backupsvc.WriteKit(f, kit)
		f.Close()
		fmt.Printf("kit written to %s — copy it somewhere safe and delete it from this machine\n", out)
	}
}

func progressPrinter() func(engine.Progress) {
	tty := term.IsTerminal(int(os.Stderr.Fd()))
	return func(p engine.Progress) {
		line := fmt.Sprintf("%-9s %d files, %s scanned, %s new, %s uploaded",
			p.Phase, p.Files, humanBytes(p.Bytes), humanBytes(p.BytesNew), humanBytes(p.Uploaded))
		if tty {
			fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
		} else {
			fmt.Fprintln(os.Stderr, line)
		}
	}
}

func backupRunCmd() *cobra.Command {
	var noPrune bool
	cmd := &cobra.Command{
		Use:   "run [TARGET...]",
		Short: "Back up now (all targets by default), then prune per retention",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			ctx, cancel := signalContext()
			defer cancel()
			names := targetNames(svc.Cfg, args)
			if len(names) == 0 {
				return errors.New("no backup targets configured")
			}
			var failed []string
			for _, name := range names {
				fmt.Fprintf(os.Stderr, "backing up to %s\n", name)
				snap, err := svc.Backup(ctx, name, progressPrinter())
				fmt.Fprintln(os.Stderr)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
					failed = append(failed, name)
					continue
				}
				printStats(snap)
				if !noPrune {
					if rep, err := svc.Prune(ctx, name, false); err != nil {
						fmt.Fprintf(os.Stderr, "  prune failed: %v\n", err)
					} else if len(rep.SnapshotsRemoved) > 0 || rep.PacksDeleted > 0 {
						fmt.Printf("  pruned %d snapshots, freed %s\n", len(rep.SnapshotsRemoved), humanBytes(rep.BytesFreed))
					}
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("backup failed for: %s", strings.Join(failed, ", "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noPrune, "no-prune", false, "skip pruning after the backup")
	return cmd
}

func printStats(s *repo.Snapshot) {
	st := s.Stats
	fmt.Printf("  snapshot %s: %d files (%s), %d unchanged\n", s.ID[:12], st.Files, humanBytes(st.BytesScanned), st.FilesUnchanged)
	fmt.Printf("  new data %s, deduplicated %s, stored %s after compression, uploaded %s in %d packs, took %s\n",
		humanBytes(st.BytesNew), humanBytes(st.BytesDeduped), humanBytes(st.BytesStored), humanBytes(st.BytesUploaded),
		st.PacksUploaded, st.Duration.Round(time.Second))
	for i, e := range st.Errors {
		if i == 5 {
			fmt.Printf("  ... and %d more skipped files\n", len(st.Errors)-5)
			break
		}
		fmt.Println("  skipped:", e)
	}
}

func backupStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show last backup per target",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "TARGET\tLOCATION\tSCHEDULE\tLAST SUCCESS\tLAST ERROR")
			for _, st := range svc.Status() {
				last := "never"
				if st.LastSuccess != nil {
					last = st.LastSuccess.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", st.Name, st.Location, st.Schedule, last, st.LastError)
			}
			tw.Flush()
			for _, w := range svc.Warnings() {
				fmt.Println("warning:", w)
			}
			return nil
		},
	}
}

func backupSnapshotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshots TARGET",
		Short: "List snapshots",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			r, _, err := svc.OpenRepo(context.Background(), args[0])
			if err != nil {
				return err
			}
			snaps, err := r.Snapshots(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTIME\tHOST\tFILES\tSIZE\tNEW")
			for _, s := range snaps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", s.ID[:12], s.Time.Local().Format("2006-01-02 15:04"), s.Host,
					s.Stats.Files, humanBytes(s.Stats.BytesScanned), humanBytes(s.Stats.BytesNew))
			}
			return tw.Flush()
		},
	}
}

func backupCheckCmd() *cobra.Command {
	var readFraction float64
	var local, thaw bool
	cmd := &cobra.Command{
		Use:   "check TARGET",
		Short: "Verify a repository (structure always; data with --read; library bit rot with --local)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			opts := engine.CheckOptions{ReadFraction: readFraction, Thaw: thaw}
			if local {
				opts.LocalSource, opts.LocalPath = backupsvc.SourceLibrary, svc.Cfg.Paths.Library
			}
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := svc.Check(ctx, args[0], opts)
			if err != nil {
				return err
			}
			fmt.Printf("snapshots %d, packs %d, packs read %d (%d blobs verified), thawing %d, files compared %d\n",
				rep.Snapshots, rep.Packs, rep.PacksRead, rep.BlobsVerified, rep.PacksThawing, rep.FilesCompared)
			if rep.OrphanPacks > 0 {
				fmt.Printf("%d unreferenced packs (%s) will be removed by prune\n", rep.OrphanPacks, humanBytes(rep.UnindexedBytes))
			}
			for _, e := range rep.Errors {
				fmt.Println("ERROR:", e)
			}
			for _, e := range rep.LocalMismatch {
				fmt.Println("LIBRARY:", e)
			}
			if !rep.OK() {
				return errors.New("check found problems")
			}
			fmt.Println("no problems found")
			return nil
		},
	}
	cmd.Flags().Float64Var(&readFraction, "read", 0, "fraction of packs to download and verify (0–1). Archived packs are skipped unless --thaw")
	cmd.Flags().BoolVar(&local, "local", false, "re-hash library files unchanged since the last snapshot to detect disk corruption")
	cmd.Flags().BoolVar(&thaw, "thaw", false, "request thawing of sampled archived packs (costs money; re-run check after they're ready)")
	return cmd
}

func backupPruneCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune TARGET",
		Short: "Forget snapshots outside the retention policy and delete unused data",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := svc.Prune(ctx, args[0], dryRun)
			if err != nil {
				return err
			}
			verb := "removed"
			if dryRun {
				verb = "would remove"
			}
			fmt.Printf("snapshots: kept %d, %s %d\n", rep.SnapshotsKept, verb, len(rep.SnapshotsRemoved))
			fmt.Printf("packs: %s %d (%s, saves ~$%.2f/month), repacked %d\n", verb, rep.PacksDeleted, humanBytes(rep.BytesFreed), rep.MonthlySavingsUSD, rep.PacksRepacked)
			if rep.PacksDeferred > 0 {
				fmt.Printf("deferred %d unused packs (%s) until their minimum storage duration passes (avoids ~$%.2f early-deletion fees)\n",
					rep.PacksDeferred, humanBytes(rep.BytesDeferred), rep.EarlyDeleteAvoidUSD)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be deleted")
	return cmd
}

func backupUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock TARGET",
		Short: "Remove stale repository locks (only if no backup is running anywhere)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			r, _, err := svc.OpenRepo(context.Background(), args[0])
			if err != nil {
				return err
			}
			n, err := r.RemoveLocks(context.Background())
			fmt.Printf("removed %d locks\n", n)
			return err
		},
	}
}

func backupKitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kit TARGET",
		Short: "Print the recovery kit for a target (without the recovery key, which is never stored)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			t, err := cfg.Target(args[0])
			if err != nil {
				return err
			}
			printKit(backupsvc.Kit{Server: cfg.Server.Name, Created: time.Now().UTC(), Target: *t}, "")
			fmt.Println("Add your recovery key (or passphrase) by hand, or make a new one with `pdrive backup new-recovery-key`.")
			return nil
		},
	}
}

func backupNewRecoveryKeyCmd() *cobra.Command {
	var kitOut string
	cmd := &cobra.Command{
		Use:   "new-recovery-key TARGET",
		Short: "Add a new recovery key (if the old one was lost); old keys keep working",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			r, t, err := svc.OpenRepo(context.Background(), args[0])
			if err != nil {
				return err
			}
			rk, err := crypto.NewRecoveryKey()
			if err != nil {
				return err
			}
			if err := r.AddKey(context.Background(), rk, crypto.LabelRecovery); err != nil {
				return err
			}
			printKit(backupsvc.Kit{Server: svc.Cfg.Server.Name, Created: time.Now().UTC(), Target: *t, RecoveryKey: rk}, kitOut)
			return nil
		},
	}
	cmd.Flags().StringVar(&kitOut, "kit-out", "", "also write the kit to this file")
	return cmd
}

func backupPasswdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "passwd",
		Short: "Change the backup passphrase on every target (recovery keys keep working)",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, closeFn, err := openBackupService()
			if err != nil {
				return err
			}
			defer closeFn()
			current, err := svc.Passphrase()
			if err != nil {
				if current, err = readSecret("Current passphrase or recovery key: ", false); err != nil {
					return err
				}
			}
			pass, err := readSecret("New backup passphrase: ", true)
			if err != nil {
				return err
			}
			for _, name := range targetNames(svc.Cfg, nil) {
				t, err := svc.Cfg.Target(name)
				if err != nil {
					return err
				}
				store, err := backupsvc.OpenStore(context.Background(), t)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				r, err := repo.Open(context.Background(), store, current, svc.Cfg.StatePath("backup", "cache"), backupsvc.Classes(t))
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				if err := r.ChangePassphrase(context.Background(), pass); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				fmt.Println("updated", name)
			}
			return backupsvc.SavePassphrase(svc.Cfg.PassphrasePath(), pass)
		},
	}
}

// ---- restore ---------------------------------------------------------------------

func restoreCmd() *cobra.Command {
	var (
		kitPath, targetName, snapshot, stateDir, libraryDir, tier string
		include                                                   []string
		meta, plan, noWait, yes, force, verify                    bool
		poll                                                      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore server metadata and/or the library from a backup",
		Long: `Restore from a backup target, either one in the config (--target) or from a
recovery kit on a new machine (--kit).

  --meta        restore ONLY the database, secrets, TLS CA and config into
                --state (fast: stored in S3 Standard) and write the config file,
                pointing it at --library. No library files are restored.
  --library DIR without --meta: restore library files into DIR (disk A).
  --plan        only show what would be restored and the estimated cost.

Deep Archive data must be thawed first (up to 48h with the Bulk tier). The
restore waits and polls; with --no-wait it requests thawing and exits — run
the same command again later. Restores are resumable.`,
		Example: `  # new machine, from the recovery kit (scripts/bootstrap.sh does this for you)
  pdrive restore --kit kit.yaml --meta --library /mnt/pdrive-a/library
  pdrive restore --kit kit.yaml --library /mnt/pdrive-a/library --plan
  pdrive restore --kit kit.yaml --library /mnt/pdrive-a/library

  # get one folder back from an older snapshot into a scratch dir
  pdrive restore --target disk-b --snapshot 3f2a --include library/users/alice/Taxes --library /tmp/recovered`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if (kitPath == "") == (targetName == "") {
				return errors.New("pass exactly one of --kit or --target")
			}
			if !meta && libraryDir == "" {
				return errors.New("nothing to restore: pass --meta and/or --library DIR")
			}
			ctx, cancel := signalContext()
			defer cancel()

			var t *config.BackupTarget
			var secret string
			fromRecoveryKey := false
			cfg, _ := config.Read(configPath)
			if kitPath != "" {
				kit, err := backupsvc.ReadKit(kitPath)
				if err != nil {
					return err
				}
				t, secret = &kit.Target, kit.RecoveryKey
				fromRecoveryKey = secret != ""
			} else {
				var err error
				if t, err = cfg.Target(targetName); err != nil {
					return err
				}
			}
			if stateDir == "" {
				stateDir = cfg.Paths.State
			}
			if secret == "" {
				if data, err := os.ReadFile(cfg.PassphrasePath()); err == nil {
					secret = strings.TrimSpace(string(data))
				} else if v := os.Getenv("PDRIVE_BACKUP_PASSPHRASE"); v != "" {
					secret = v
				} else {
					var err error
					if secret, err = readSecret("Backup passphrase or recovery key: ", false); err != nil {
						return err
					}
				}
			}
			store, err := backupsvc.OpenStore(ctx, t)
			if err != nil {
				return err
			}
			r, err := repo.Open(ctx, store, secret, filepath.Join(stateDir, "backup", "cache"), backupsvc.Classes(t))
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "opened backup on %s\n", backupsvc.DescribeTarget(t))

			if meta {
				if plan {
					return errors.New("--plan applies to library restores; run it without --meta")
				}
				if libraryDir == "" {
					return errors.New("--meta needs --library so the restored config points at the right disk")
				}
				abs, _ := filepath.Abs(libraryDir)
				fmt.Fprintln(os.Stderr, "restoring server metadata...")
				err := backupsvc.RestoreMeta(ctx, r, backupsvc.RestoreMetaOptions{
					Snapshot: snapshot, StateDir: stateDir, ConfigPath: configPath, LibraryPath: abs, Target: *t, Force: force,
				})
				if err != nil {
					return err
				}
				passPath := filepath.Join(stateDir, "secrets", "backup-passphrase")
				if _, err := os.Stat(passPath); err != nil {
					switch {
					case os.Getenv("PDRIVE_BACKUP_PASSPHRASE") != "":
						backupsvc.SavePassphrase(passPath, os.Getenv("PDRIVE_BACKUP_PASSPHRASE"))
					case !fromRecoveryKey:
						backupsvc.SavePassphrase(passPath, secret) // the passphrase itself was used
					default:
						fmt.Println("scheduled backups need a passphrase: run `pdrive backup passwd` (it accepts the recovery key)")
					}
				}
				fmt.Printf("metadata restored to %s, config written to %s\n", stateDir, configPath)
				fmt.Println("the server stays read-only until the library restore completes")
				return nil
			}
			abs, _ := filepath.Abs(libraryDir)
			opts := engine.RestoreOptions{
				Snapshot: snapshot, Targets: map[string]string{backupsvc.SourceLibrary: abs}, Include: include,
				Tier: tier, PollInterval: poll, NoWait: noWait, Verify: verify, Pricing: engine.DefaultPricing(),
				Local: t.Type == "local", CacheDir: filepath.Join(stateDir, "backup", "restore-cache"),
			}
			p, err := engine.PlanRestore(ctx, r, opts)
			if err != nil {
				return err
			}
			fmt.Print(p.Summary())
			if err := os.MkdirAll(abs, 0o750); err != nil {
				return err
			}
			if free, _ := volume.Usage(abs); free > 0 && uint64(p.RemainingBytes()) > free {
				msg := fmt.Sprintf("not enough space in %s: need %s, %s free", abs, humanBytes(p.RemainingBytes()), humanBytes(int64(free)))
				if !force {
					return errors.New(msg + " (use --force to try anyway)")
				}
				fmt.Fprintln(os.Stderr, "warning:", msg)
			}
			if plan {
				return nil
			}
			if !yes && term.IsTerminal(int(os.Stdin.Fd())) {
				fmt.Print("proceed? [y/N] ")
				line, _ := stdin.ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
					return errors.New("aborted")
				}
			}
			tty := term.IsTerminal(int(os.Stderr.Fd()))
			last := time.Time{}
			opts.Progress = func(rp engine.RestoreProgress) {
				if time.Since(last) < time.Second && rp.Phase != "done" {
					return
				}
				last = time.Now()
				line := fmt.Sprintf("%s: %d/%d files, %s/%s", rp.Phase, rp.FilesDone, rp.FilesTotal, humanBytes(rp.BytesDone), humanBytes(rp.BytesTotal))
				if rp.Phase == "thawing" {
					line = fmt.Sprintf("thawing: %d/%d packs ready (checking every %s)", rp.PacksReady, rp.PacksTotal, poll)
				}
				if tty {
					fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
				} else {
					fmt.Fprintln(os.Stderr, line)
				}
			}
			err = backupsvc.RestoreLibrary(ctx, r, p, opts, stateDir)
			fmt.Fprintln(os.Stderr)
			if errors.Is(err, engine.ErrThawPending) {
				fmt.Println(err)
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Printf("library restored to %s\n", abs)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&kitPath, "kit", "", "recovery kit file (new machine)")
	f.StringVar(&targetName, "target", "", "backup target from the config")
	f.StringVar(&snapshot, "snapshot", "latest", "snapshot id (prefix) or latest")
	f.StringVar(&stateDir, "state", "", "state directory (default from config)")
	f.StringVar(&libraryDir, "library", "", "restore library files into this directory")
	f.StringSliceVar(&include, "include", nil, "only restore these snapshot paths, e.g. library/users/alice/Photos")
	f.BoolVar(&meta, "meta", false, "restore database, secrets, TLS CA and config")
	f.BoolVar(&plan, "plan", false, "show the plan and cost estimate only")
	f.StringVar(&tier, "tier", "Bulk", "archive retrieval tier: Bulk (cheapest, ≤48h) | Standard (≤12h) | Expedited (Glacier Flexible only)")
	f.BoolVar(&noWait, "no-wait", false, "request thawing and exit instead of waiting")
	f.BoolVar(&yes, "yes", false, "don't ask for confirmation")
	f.BoolVar(&force, "force", false, "overwrite an existing database when restoring --meta")
	f.BoolVar(&verify, "verify", false, "re-hash files that look already restored")
	f.DurationVar(&poll, "poll", 15*time.Minute, "how often to check thaw progress")
	return cmd
}
