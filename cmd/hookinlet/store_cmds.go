// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/hookinlet/hookinlet/internal/config"
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
	store, err := sqlite.Open(ctx, data.DataDir)
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
