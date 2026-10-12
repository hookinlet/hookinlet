// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/config"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// verifierSecretEnv carries the verifier secret. It is read from the
// environment only, never from a flag, because flags are visible in process
// listings and shell history.
const verifierSecretEnv = config.EnvPrefix + "VERIFIER_SECRET"

// migrateCmd opens the data directory, which applies any pending migrations,
// and reports the schema version.
func migrateCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	resolve := config.AddDataFlags(fs, getenv)
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	data, err := resolve()
	if err != nil {
		return configError(stderr, err)
	}

	ctx := context.Background()
	store, err := sqlite.Open(ctx, data.DataDir, sqlite.WithExclusiveLock(), sqlite.WithLogger(slog.New(slog.NewTextHandler(stderr, nil))))
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	defer func() { _ = store.Close() }()
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	if applied := store.AppliedMigrations(); len(applied) > 0 {
		fmt.Fprintf(stdout, "applied migrations %v; schema is now at version %d\n", applied, version)
	} else {
		fmt.Fprintf(stdout, "schema is up to date at version %d\n", version)
	}
	return 0
}

// rekeyCmd seals every stored verifier secret again under the master key, which is
// the new one, opening each with --old-master-key (HOOKINLET_OLD_MASTER_KEY). Without
// an old key it uses the current one, which brings secrets written by an earlier
// release up to the current layout. Stop the server first.
func rekeyCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet rekey", flag.ContinueOnError)
	fs.SetOutput(stderr)
	resolve := config.AddDataFlags(fs, getenv)
	oldKey := fs.String("old-master-key", "", "the master key the secrets are sealed under now (prefer the environment variable "+config.EnvPrefix+"OLD_MASTER_KEY: flags are visible in process listings); default: the master key")
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	data, err := resolve()
	if err != nil {
		return configError(stderr, err)
	}
	if data.MasterKey == "" {
		fmt.Fprintf(stderr, "hookinlet: the new master key is required (--master-key or %sMASTER_KEY)\n", config.EnvPrefix)
		return exitUsage
	}
	newBox, err := secretbox.New([]byte(data.MasterKey.Reveal()))
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitUsage
	}
	oldBox := newBox
	old := *oldKey
	if old == "" {
		old = getenv(config.EnvPrefix + "OLD_MASTER_KEY")
	}
	if old != "" {
		k, err := config.ParseMasterKey(old)
		if err != nil {
			fmt.Fprintf(stderr, "hookinlet: --old-master-key: %v\n", err)
			return exitUsage
		}
		if oldBox, err = secretbox.New([]byte(k.Reveal())); err != nil {
			fmt.Fprintf(stderr, "hookinlet: %v\n", err)
			return exitUsage
		}
	}
	if _, err := os.Stat(filepath.Join(data.DataDir, sqlite.DBFileName)); err != nil {
		fmt.Fprintf(stderr, "hookinlet: there is no database in %s; check --data-dir\n", data.DataDir)
		return exitRuntime
	}
	ctx := context.Background()
	store, err := sqlite.Open(ctx, data.DataDir, sqlite.WithExclusiveLock())
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	defer func() { _ = store.Close() }()
	res, err := (&admin.Service{Store: store, Box: newBox}).Rekey(ctx, oldBox)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintf(stdout, "sealed %d secret(s) under key %s; %d already were\n", res.Resealed, newBox.KeyID(), res.Unchanged)
	if res.Skipped > 0 {
		fmt.Fprintf(stderr, "hookinlet: %d inbox(es) were changed while this ran and were left alone; stop the server and run rekey again\n", res.Skipped)
		return exitRuntime
	}
	return 0
}

// renewEpochsCmd gives every inbox a new epoch, in the data directory. It is for
// the moment after a database is restored from a copy of the data directory (or
// one snapshot is restored a second time): the restored server may hand out
// sequence numbers that differ from the ones its predecessor handed out, and a new
// epoch is how a client holding an old cursor finds out. Snapshots from `hookinlet
// backup` and GET /v1/admin/backup already have new epochs. Stop the server first.
func renewEpochsCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet renew-epochs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	resolve := config.AddDataFlags(fs, getenv)
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	data, err := resolve()
	if err != nil {
		return configError(stderr, err)
	}
	if _, err := os.Stat(filepath.Join(data.DataDir, sqlite.DBFileName)); err != nil {
		fmt.Fprintf(stderr, "hookinlet: there is no database in %s; check --data-dir\n", data.DataDir)
		return exitRuntime
	}
	ctx := context.Background()
	store, err := sqlite.Open(ctx, data.DataDir, sqlite.WithExclusiveLock())
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	defer func() { _ = store.Close() }()
	if err := store.RenewEpochs(ctx); err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintln(stdout, "every inbox has a new epoch; readers holding an old cursor will be told to start again")
	return 0
}

// parseFlags parses args. It returns done == true with the exit code when the
// caller should stop: after -h, or after a parse error the flag package has
// already reported.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) (code int, done bool) {
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0, true
	case err != nil:
		return exitUsage, true
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hookinlet: unexpected argument %q\n", fs.Arg(0))
		return exitUsage, true
	}
	return 0, false
}

func configError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "hookinlet: invalid configuration:\n  %s\n", strings.ReplaceAll(err.Error(), "\n", "\n  "))
	return exitUsage
}
