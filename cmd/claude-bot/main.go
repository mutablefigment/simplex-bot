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

	// 0o700 — defense-in-depth. The systemd unit already restricts via
	// User=claude-bot + ProtectHome=tmpfs, but the bot shouldn't rely on that
	// alone: the workspace can hold files Claude writes via the Write/Edit
	// tools, and the inbox holds user-sent attachments. Both can be sensitive.
	// Chmod after MkdirAll to repair perms on dirs created by a previous
	// version that used 0o755.
	must(os.MkdirAll(cfg.Claude.Workspace, 0o700))
	must(os.Chmod(cfg.Claude.Workspace, 0o700))
	must(os.MkdirAll(cfg.Storage.InboxDir, 0o700))
	must(os.Chmod(cfg.Storage.InboxDir, 0o700))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(ctx, cfg.Storage.DBPath)
	must(err)
	defer st.Close()

	sx := simplex.New(cfg.Simplex, log)
	cr := claude.NewRunner(cfg.Claude, log)
	b := bot.New(cfg, log, sx, cr, st)

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
