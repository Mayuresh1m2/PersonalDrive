package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/dupes"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/library"
)

func reindexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the file index from what is on the library disk (server must be stopped)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(true)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := a.Lib.Reindex(ctx, func(s string) { fmt.Fprintln(os.Stderr, s) })
			if err != nil {
				return err
			}
			fmt.Printf("scanned %d, added %d, updated %d, removed %d, hashed %s\n",
				rep.Scanned, rep.Added, rep.Updated, rep.Removed, humanBytes(rep.HashedBytes))
			for _, o := range rep.UnknownOwners {
				fmt.Printf("warning: %s has no matching user/space; create it and reindex again\n", o)
			}
			if rep.Added+rep.Updated > 0 {
				fmt.Println("thumbnails and metadata will be generated next time the server runs")
			}
			return nil
		},
	}
}

func importCmd() *cobra.Command {
	var user, space, dest string
	cmd := &cobra.Command{
		Use:   "import SRC...",
		Short: "Copy existing folders into a library (server must be stopped). Sources are never modified.",
		Example: `  pdrive import --user mayuresh --dest "Old Laptop" /media/old-disk/Pictures
  pdrive import --space "Family Photos" /media/old-disk/Trips`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (user == "") == (space == "") {
				return errors.New("pass exactly one of --user or --space")
			}
			a, err := openApp(true)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx, cancel := signalContext()
			defer cancel()

			var acc library.Access
			if user != "" {
				u, err := a.DB.UserByName(ctx, user)
				if err != nil {
					return fmt.Errorf("user %q: %w", user, err)
				}
				acc = a.Lib.UserAccess(u)
			} else {
				sp, err := a.DB.SpaceByName(ctx, space)
				if err != nil {
					return fmt.Errorf("space %q: %w", space, err)
				}
				if acc, err = a.Lib.SpaceAccess(ctx, sp.ID); err != nil {
					return err
				}
			}
			if err := a.Guard.Writable(); err != nil {
				return err
			}
			total := &library.ImportReport{}
			for _, src := range args {
				fmt.Fprintln(os.Stderr, "importing", src)
				rep, err := a.Lib.Import(ctx, acc, src, dest, func(s string) { fmt.Fprintln(os.Stderr, "  "+s) })
				if err != nil {
					return err
				}
				total.Files += rep.Files
				total.Imported += rep.Imported
				total.Unchanged += rep.Unchanged
				total.Conflicts += rep.Conflicts
				total.Linked += rep.Linked
				total.Bytes += rep.Bytes
				total.LinkedBytes += rep.LinkedBytes
				total.Errors = append(total.Errors, rep.Errors...)
			}
			fmt.Printf("files: %d  imported: %d (%s)  already there: %d  conflicts: %d\n",
				total.Files, total.Imported, humanBytes(total.Bytes), total.Unchanged, total.Conflicts)
			fmt.Printf("dedup: %d files (%s) share storage with existing copies\n", total.Linked, humanBytes(total.LinkedBytes))
			for _, e := range total.Errors {
				fmt.Println("error:", e)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "import into this user's library")
	cmd.Flags().StringVar(&space, "space", "", "import into this shared space")
	cmd.Flags().StringVar(&dest, "dest", "", "folder inside the library to import into")
	return cmd
}

func dupesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dupes", Short: "Find duplicate files"}
	var asJSON bool
	var top int
	var minKB int64
	scan := &cobra.Command{
		Use:   "scan PATH...",
		Short: "Report exact duplicates and how much space dedup would save (read-only)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			rep, err := dupes.Scan(ctx, args, minKB<<10, func(s string) { fmt.Fprintln(os.Stderr, s) })
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			printDupes(rep, top)
			return nil
		},
	}
	scan.Flags().BoolVar(&asJSON, "json", false, "print the full report as JSON")
	scan.Flags().IntVar(&top, "top", 20, "number of duplicate groups to list")
	scan.Flags().Int64Var(&minKB, "min-kb", 1, "ignore files smaller than this")
	cmd.AddCommand(scan)
	return cmd
}

func printDupes(rep *dupes.Report, top int) {
	fmt.Printf("\nscanned %d files, %s\n", rep.Files, humanBytes(rep.TotalBytes))
	pct := 0.0
	if rep.TotalBytes > 0 {
		pct = 100 * float64(rep.WastedBytes) / float64(rep.TotalBytes)
	}
	fmt.Printf("duplicate groups: %d — dedup would save %s (%.1f%%)\n\n", len(rep.Groups), humanBytes(rep.WastedBytes), pct)
	for i, g := range rep.Groups {
		if i >= top {
			fmt.Printf("... and %d more groups (use --json for all)\n", len(rep.Groups)-top)
			break
		}
		fmt.Printf("%s x%d (saves %s)\n", humanBytes(g.Size), len(g.Paths), humanBytes(g.Wasted()))
		for _, p := range g.Paths {
			fmt.Println("   ", p)
		}
	}
	if len(rep.Errors) > 0 {
		fmt.Printf("\n%d paths could not be read (permissions?)\n", len(rep.Errors))
	}
}
