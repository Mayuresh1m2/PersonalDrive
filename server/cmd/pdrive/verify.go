package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
)

// verifyCopyCmd answers "is everything in this folder safely in the
// library?" before you delete an old copy anywhere else. Read-only.
func verifyCopyCmd() *cobra.Command {
	var showAll bool
	cmd := &cobra.Command{
		Use:   "verify-copy SRC...",
		Short: "Check that every file under SRC exists in the library (by exact content). Read-only.",
		Long: `Hashes every file under SRC and checks the library holds identical content
(anywhere, for any user). Run this before deleting an old disk, phone backup
or cloud export: if it reports 0 missing, every file is in the library.
It doesn't check backups; use 'pdrive backup check' for that.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx, cancel := signalContext()
			defer cancel()
			var total, missing int
			var totalBytes, missingBytes int64
			for _, src := range args {
				err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						fmt.Println("UNREADABLE", p, err)
						missing++
						return nil
					}
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if !d.Type().IsRegular() || importSkip(d.Name()) {
						return nil
					}
					sum, n, err := storage.HashFile(p)
					if err != nil {
						fmt.Println("UNREADABLE", p, err)
						missing++
						return nil
					}
					total++
					totalBytes += n
					have, err := a.DB.HaveContent(context.Background(), []string{sum}, nil)
					if err != nil {
						return err
					}
					if !have[sum] {
						missing++
						missingBytes += n
						fmt.Println("MISSING", p)
					} else if showAll {
						fmt.Println("ok     ", p)
					}
					if total%500 == 0 {
						fmt.Fprintf(os.Stderr, "checked %d files...\n", total)
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			fmt.Printf("\n%d files (%s) checked: %d present in the library, %d missing (%s)\n",
				total, humanBytes(totalBytes), total-missing, missing, humanBytes(missingBytes))
			if missing > 0 {
				return fmt.Errorf("%d files are NOT in the library — don't delete the source yet", missing)
			}
			fmt.Println("every file is in the library")
			return nil
		},
	}
	cmd.Flags().BoolVar(&showAll, "all", false, "also list files that are present")
	return cmd
}

func importSkip(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", "desktop.ini", ".localized":
		return true
	}
	return len(name) > 2 && name[:2] == "._"
}
