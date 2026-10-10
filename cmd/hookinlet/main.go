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
	"github.com/hookinlet/hookinlet/pkg/server"
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

	srv, err := server.New(server.Options{
		Addr:            cfg.ListenAddr,
		Logger:          log,
		ShutdownTimeout: cfg.ShutdownTimeout,
	})
	if err != nil {
		log.Error("server setup failed", "err", err)
		return exitRuntime
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "err", err)
		return exitRuntime
	}
	log.Info("stopped")
	return 0
}

func newLogger(w io.Writer, cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
