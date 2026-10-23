package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/api"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/storage"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/tlsutil"
)

// stdin is shared so successive prompts fed from a pipe each get one line.
var stdin = bufio.NewReader(os.Stdin)

// readPassword prompts twice on a terminal, or reads one line from stdin.
func readPassword(prompt string) (string, error) {
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
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passwords don't match")
	}
	return string(a), nil
}

func userCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Manage users"}

	var admin bool
	var display string
	var quotaGB int64
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Create a user (password read from the terminal or stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := storage.CleanName(args[0])
			if err != nil {
				return err
			}
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			pw, err := readPassword("Password for " + name + ": ")
			if err != nil {
				return err
			}
			role := "member"
			if admin {
				role = "admin"
			}
			u, err := a.Auth.CreateUser(context.Background(), name, display, pw, role)
			if err != nil {
				return err
			}
			if quotaGB > 0 {
				u.QuotaBytes = quotaGB << 30
				if err := a.DB.UpdateUser(context.Background(), u); err != nil {
					return err
				}
			}
			fmt.Printf("created %s %s (id %d)\n", role, u.Username, u.ID)
			return nil
		},
	}
	add.Flags().BoolVar(&admin, "admin", false, "make the user an admin")
	add.Flags().StringVar(&display, "display-name", "", "display name")
	add.Flags().Int64Var(&quotaGB, "quota-gb", 0, "storage quota in GiB (0 = unlimited)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List users",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			users, err := a.DB.ListUsers(context.Background())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tUSERNAME\tROLE\tUSED\tQUOTA\tDISABLED")
			for _, u := range users {
				used, _ := a.DB.UserUsage(context.Background(), u.ID)
				quota := "-"
				if u.QuotaBytes > 0 {
					quota = humanBytes(u.QuotaBytes)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%v\n", u.ID, u.Username, u.Role, humanBytes(used), quota, u.Disabled)
			}
			return tw.Flush()
		},
	}

	passwd := &cobra.Command{
		Use:   "passwd NAME",
		Short: "Set a user's password",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			u, err := a.DB.UserByName(context.Background(), args[0])
			if err != nil {
				return err
			}
			pw, err := readPassword("New password for " + u.Username + ": ")
			if err != nil {
				return err
			}
			return a.Auth.SetPassword(context.Background(), u.ID, pw)
		},
	}

	setDisabled := func(use string, disabled bool) *cobra.Command {
		return &cobra.Command{
			Use:   use + " NAME",
			Short: strings.ToUpper(use[:1]) + use[1:] + " a user",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := openApp(false)
				if err != nil {
					return err
				}
				defer a.Close()
				u, err := a.DB.UserByName(context.Background(), args[0])
				if err != nil {
					return err
				}
				u.Disabled = disabled
				return a.DB.UpdateUser(context.Background(), u)
			},
		}
	}
	cmd.AddCommand(add, list, passwd, setDisabled("disable", true), setDisabled("enable", false))
	return cmd
}

func spaceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "space", Short: "Manage shared spaces (e.g. Family Photos)"}
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Create a shared space",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := storage.CleanName(args[0])
			if err != nil {
				return err
			}
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			sp, err := a.DB.CreateSpace(context.Background(), name, 0)
			if err != nil {
				return err
			}
			fmt.Printf("created space %s (id %d)\n", sp.Name, sp.ID)
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List spaces and members",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			spaces, err := a.DB.ListSpaces(context.Background(), 0)
			if err != nil {
				return err
			}
			for _, sp := range spaces {
				ms, _ := a.DB.SpaceMembers(context.Background(), sp.ID)
				var parts []string
				for _, m := range ms {
					parts = append(parts, m.Username+":"+m.Permission)
				}
				fmt.Printf("%d\t%s\t%s\n", sp.ID, sp.Name, strings.Join(parts, ", "))
			}
			return nil
		},
	}
	member := &cobra.Command{
		Use:   "member SPACE USER read|write|remove",
		Short: "Grant or remove a user's access to a space",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := context.Background()
			sp, err := a.DB.SpaceByName(ctx, args[0])
			if err != nil {
				return fmt.Errorf("space %q: %w", args[0], err)
			}
			u, err := a.DB.UserByName(ctx, args[1])
			if err != nil {
				return fmt.Errorf("user %q: %w", args[1], err)
			}
			switch args[2] {
			case "read", "write":
				return a.DB.SetSpaceMember(ctx, sp.ID, u.ID, args[2])
			case "remove":
				return a.DB.RemoveSpaceMember(ctx, sp.ID, u.ID)
			}
			return errors.New("permission must be read, write or remove")
		},
	}
	cmd.AddCommand(add, list, member)
	return cmd
}

func pairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pair USER",
		Short: "Show a QR code a phone or laptop can scan to sign in as USER",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp(false)
			if err != nil {
				return err
			}
			defer a.Close()
			u, err := a.DB.UserByName(context.Background(), args[0])
			if err != nil {
				return err
			}
			tok, exp, err := a.Auth.CreatePairingToken(context.Background(), u.ID)
			if err != nil {
				return err
			}
			srv := api.New(api.Deps{Cfg: a.Cfg})
			p := api.PairingPayload{V: 1, Name: a.Cfg.Server.Name, URLs: srv.URLs(), Token: tok}
			if a.Cfg.Server.TLS {
				m, err := tlsutil.Load(a.Cfg.StatePath("tls"))
				if err != nil {
					return err
				}
				p.CASHA256 = m.CAFingerprint()
			}
			data, _ := json.Marshal(p)
			qrterminal.GenerateHalfBlock(string(data), qrterminal.L, os.Stdout)
			fmt.Printf("\nScan with the PersonalDrive app to sign in as %s. Expires %s.\n", u.Username, exp.Local().Format("15:04"))
			fmt.Printf("Manual pairing payload:\n%s\n", data)
			return nil
		},
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
