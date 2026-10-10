// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Command hookinlet is a webhook inbox for AI agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hookinlet/hookinlet/internal/config"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/server"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes: 1 is a runtime failure, 2 is a usage or configuration error.
const (
	exitRuntime = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], getenv, stderr)
	case "migrate":
		return migrateCmd(args[1:], getenv, stdout, stderr)
	case "inbox":
		return inboxCmd(args[1:], getenv, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "hookinlet: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: hookinlet <command> [flags]

Commands:
  serve     Run the HTTP server (see "hookinlet serve -h" for settings)
  migrate   Apply pending database migrations and report the schema version
  inbox     Manage inboxes directly in the data directory (inbox create)
  version   Print the version
  help      Show this help

Every setting is a flag and an environment variable: --listen-addr is also
HOOKINLET_LISTEN_ADDR. Flags win over the environment.
`)
}

func serve(args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.Load(args, getenv, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, config.ErrUsage):
		return exitUsage // the flag package already explained
	case err != nil:
		fmt.Fprintf(stderr, "hookinlet: invalid configuration:\n  %s\n",
			strings.ReplaceAll(err.Error(), "\n", "\n  "))
		return exitUsage
	}

	log := newLogger(stderr, cfg)
	log.Info("starting", "version", version, "config", cfg)

	srv, closeStore, err := buildServer(context.Background(), cfg, log)
	if err != nil {
		log.Error("server setup failed", "err", err)
		return exitRuntime
	}
	defer closeStore()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "err", err)
		return exitRuntime
	}
	log.Info("stopped")
	return 0
}

// buildServer opens the data directory (applying any pending migrations) and
// assembles the server around it. The returned function closes the store and
// must be called after the server has stopped.
func buildServer(ctx context.Context, cfg *config.Config, log *slog.Logger) (*server.Server, func(), error) {
	store, err := sqlite.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	closeStore := func() {
		if err := store.Close(); err != nil {
			log.Error("closing the store failed", "err", err)
		}
	}
	box, err := secretbox.New([]byte(cfg.MasterKey.Reveal()))
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	srv, err := server.New(server.Options{
		Addr:               cfg.ListenAddr,
		Logger:             log,
		ShutdownTimeout:    cfg.ShutdownTimeout,
		Store:              store,
		SecretBox:          box,
		Authenticator:      token.NewAuthenticator(store, cfg.AdminToken.Reveal()),
		Notifier:           local.New(),
		MaxStreams:         cfg.MaxStreams,
		MaxStreamsPerInbox: cfg.MaxStreamsPerInbox,
		SSEHeartbeat:       cfg.SSEHeartbeat,
		SSERecheckInterval: cfg.SSERecheckInterval,
		TrustedProxies:     cfg.TrustedProxies,
	})
	if err != nil {
		closeStore()
		return nil, nil, err
	}
	return srv, closeStore, nil
}

func newLogger(w io.Writer, cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
