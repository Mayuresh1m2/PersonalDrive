// Command pdrived runs the PersonalDrive server (equivalent to `pdrive serve`).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mayuresh1m2/PersonalDrive/server/internal/app"
	"github.com/Mayuresh1m2/PersonalDrive/server/internal/config"
)

func main() {
	configPath := flag.String("config", config.DefaultPath(), "config file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	a, err := app.Open(cfg, log, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer a.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := a.Serve(ctx, nil); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
