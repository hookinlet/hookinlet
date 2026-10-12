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
	// Secrets may come from files (HOOKINLET_ADMIN_TOKEN_FILE and the like). The
	// commands that use no secret do not read them: a container's health check must
	// not fail because a secret file is unreadable to it.
	switch args[0] {
	case "version", "help", "-h", "--help", "healthcheck":
	default:
		var err error
		if getenv, err = config.WithSecretFiles(getenv); err != nil {
			fmt.Fprintf(stderr, "hookinlet: %v\n", err)
			return exitUsage
		}
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], getenv, stderr)
	case "migrate":
		return migrateCmd(args[1:], getenv, stdout, stderr)
	case "inbox":
		return inboxCmd(args[1:], getenv, stdout, stderr)
	case "token":
		return tokenCmd(args[1:], getenv, stdout, stderr)
	case "retention":
		return retentionCmd(args[1:], getenv, stdout, stderr)
	case "apply":
		return applyCmd(args[1:], getenv, stdout, stderr)
	case "healthcheck":
		return healthcheckCmd(args[1:], getenv, stdout, stderr)
	case "rekey":
		return rekeyCmd(args[1:], getenv, stdout, stderr)
	case "renew-epochs":
		return renewEpochsCmd(args[1:], getenv, stdout, stderr)
	case "backup":
		return backupCmd(args[1:], getenv, stdout, stderr)
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

// usageWith gives a command that sets no usage of its own a synopsis above its flags,
// written to wherever the flag set's output goes.
func usageWith(fs *flag.FlagSet, synopsis string) {
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s\n\n", synopsis)
		fs.PrintDefaults()
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: hookinlet <command> [flags]

Commands:
  serve         Run the HTTP server (see "hookinlet serve -h" for settings)
  migrate       Apply pending database migrations and report the schema version
  inbox         Manage inboxes: create, list, show, update, set-secret, rotate-ingest-key, delete
  token         Manage agent tokens: create, list, revoke
  retention     Apply retention policies now (retention run)
  apply         Bring inboxes to the state a YAML file declares (apply -f FILE)
  healthcheck   Ask a running server if it is ready (for a container HEALTHCHECK)
  rekey         Seal the stored secrets under a new master key (server stopped)
  renew-epochs  Give every inbox a new epoch, after restoring from a copy (server stopped)
  backup        Write a consistent copy of the database (backup --out FILE)
  version       Print the version
  help          Show this help

Every setting is a flag and an environment variable: --listen-addr is also
HOOKINLET_LISTEN_ADDR. Flags win over the environment.
The secrets (HOOKINLET_ADMIN_TOKEN, HOOKINLET_MASTER_KEY, HOOKINLET_OLD_MASTER_KEY,
HOOKINLET_VERIFIER_SECRET) can instead be read from a file named by the same variable
with _FILE added, such as HOOKINLET_MASTER_KEY_FILE=/run/secrets/master_key.

The inbox, token, apply, retention, and backup commands call a running server's
admin API when given --url (with the admin token in HOOKINLET_ADMIN_TOKEN), and
otherwise work directly in the data directory (--data-dir).
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

	// A master key that is not the one the data was sealed under is found now, not by
	// the first delivery that needs a secret.
	if err := srv.CheckSecrets(context.Background()); err != nil {
		log.Error("refusing to start", "err", err)
		fmt.Fprintf(stderr, "hookinlet: %v\n\n"+
			"The master key (HOOKINLET_MASTER_KEY) must be the one the secrets were sealed under.\n"+
			"  - If this is the wrong key, start again with the right one.\n"+
			"  - If the key was rotated on purpose, re-seal the secrets under the new one:\n"+
			"      hookinlet rekey --old-master-key OLD   (new key in HOOKINLET_MASTER_KEY)\n"+
			"  - If the old key is lost, set each affected secret again:\n"+
			"      hookinlet inbox set-secret SLUG   (works in the data directory with the new key)\n", err)
		return exitRuntime
	}

	if n := srv.LegacySecrets(context.Background()); n > 0 {
		log.Warn("some verifier secrets are in the sealed format of an earlier release, which binds them to no inbox; run `hookinlet rekey` (server stopped) to bring them up to date", "secrets", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first signal starts the shutdown. Once it has, the default behaviour comes
	// back, so a second one ends the process at once if the shutdown is stuck or the
	// drain is longer than anyone cares to wait.
	go func() { <-ctx.Done(); stop() }()
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
	store, err := sqlite.Open(ctx, cfg.DataDir, sqlite.WithLogger(log), sqlite.WithExclusiveLock())
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
		DisableMCP:         !cfg.MCPEnabled,
		DisableAdmin:       !cfg.AdminEnabled,
		AdminAddr:          cfg.AdminListenAddr,
		Version:            version,
		PublicBaseURL:      cfg.PublicBaseURL,
		RetentionInterval:  cfg.RetentionInterval,
		MCPInlineBodyBytes: cfg.MCPInlineBodyBytes,
		IngestMemoryBudget: ingestBudget(cfg.IngestMemoryBudget),
		DrainDelay:         cfg.DrainDelay,
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

// ingestBudget maps the setting, where 0 means no bound, to the server option,
// where 0 means the default and a negative number means no bound.
func ingestBudget(n int64) int64 {
	if n == 0 {
		return -1
	}
	return n
}
