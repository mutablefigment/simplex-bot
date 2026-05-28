package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"claude-bot/internal/bot"
	"claude-bot/internal/claude"
	"claude-bot/internal/config"
	"claude-bot/internal/logging"
	"claude-bot/internal/simplex"
	"claude-bot/internal/store"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "/etc/claude-bot/config.toml", "path to config TOML")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	must(err)

	log := logging.New(cfg.Log)
	log.Info("starting", "version", version, "config", *cfgPath)

	must(os.MkdirAll(cfg.Claude.Workspace, 0o755))
	must(os.MkdirAll(cfg.Storage.InboxDir, 0o755))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(ctx, cfg.Storage.DBPath)
	must(err)
	defer st.Close()

	sx := simplex.New(cfg.Simplex, log)
	cr := claude.NewRunner(cfg.Claude, log)
	b := bot.New(cfg, log, version, sx, cr, st)

	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("bot exited", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown clean")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-bot:", err)
		os.Exit(1)
	}
}
