// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/hookinlet/hookinlet/internal/config"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
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

// inboxCmd dispatches the inbox subcommands. They write straight to the data
// directory and are temporary: the admin API and a client for it replace them.
func inboxCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stderr, "Usage: hookinlet inbox create --slug SLUG [flags]\n")
		if len(args) == 0 {
			return exitUsage
		}
		return 0
	}
	if args[0] != "create" {
		fmt.Fprintf(stderr, "hookinlet: unknown inbox command %q\n", args[0])
		return exitUsage
	}
	return inboxCreate(args[1:], getenv, stdout, stderr)
}

func inboxCreate(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	resolve := config.AddDataFlags(fs, getenv)
	var (
		slug       = fs.String("slug", "", "inbox slug: 3-64 characters from a-z, 0-9, and hyphen (required)")
		name       = fs.String("name", "", "display name (default: the slug)")
		verifier   = fs.String("verifier", "none", "none, token, hmac, stripe, github, slack, or standardwebhooks")
		onFail     = fs.String("on-verify-fail", inbox.OnVerifyFailReject, "reject or store")
		vconfig    = fs.String("verifier-config", "{}", "verifier parameters as a JSON object")
		maxBody    = fs.Int64("max-body-bytes", 0, "largest accepted body; 0 means the default of 1 MiB")
		retDays    = fs.Int("retention-days", 0, "delete messages older than this many days; 0 keeps them")
		retCount   = fs.Int64("retention-max-count", 0, "keep at most this many newest messages; 0 means no limit")
		eventPoint = fs.String("event-type-pointer", "", "JSON pointer to the event type in JSON bodies, such as /type")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: hookinlet inbox create --slug SLUG [flags]\n\nThe verifier secret, if any, is read from %s.\n\n", verifierSecretEnv)
		fs.PrintDefaults()
	}
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	data, err := resolve()
	if err != nil {
		return configError(stderr, err)
	}

	if *slug == "" {
		fmt.Fprintln(stderr, "hookinlet: --slug is required")
		return exitUsage
	}
	if !inbox.KnownVerifier(*verifier) {
		fmt.Fprintf(stderr, "hookinlet: unknown verifier %q (known: none, token, hmac, stripe, github, slack, standardwebhooks)\n", *verifier)
		return exitUsage
	}
	if !json.Valid([]byte(*vconfig)) {
		fmt.Fprintln(stderr, "hookinlet: --verifier-config is not valid JSON")
		return exitUsage
	}

	var sealed []byte
	if secret := getenv(verifierSecretEnv); secret != "" {
		if data.MasterKey == "" {
			fmt.Fprintf(stderr, "hookinlet: %s is set, so --master-key (%s) is required to seal it\n",
				verifierSecretEnv, config.EnvPrefix+"MASTER_KEY")
			return exitUsage
		}
		box, err := secretbox.New([]byte(data.MasterKey.Reveal()))
		if err != nil {
			fmt.Fprintf(stderr, "hookinlet: %v\n", err)
			return exitRuntime
		}
		if sealed, err = box.Seal([]byte(secret)); err != nil {
			fmt.Fprintf(stderr, "hookinlet: %v\n", err)
			return exitRuntime
		}
	}

	agentToken, hash, err := token.Generate()
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	displayName := *name
	if displayName == "" {
		displayName = *slug
	}
	in := &inbox.Inbox{
		Slug:              *slug,
		Name:              displayName,
		Verifier:          *verifier,
		VerifierConfig:    json.RawMessage(*vconfig),
		VerifierSecret:    sealed,
		OnVerifyFail:      *onFail,
		AgentTokenHash:    hash,
		MaxBodyBytes:      *maxBody,
		RetentionDays:     *retDays,
		RetentionMaxCount: *retCount,
		EventTypePointer:  *eventPoint,
	}

	ctx := context.Background()
	store, err := sqlite.Open(ctx, data.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	defer func() { _ = store.Close() }()

	switch err := store.CreateInbox(ctx, in); {
	case errors.Is(err, inbox.ErrInvalid):
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitUsage
	case errors.Is(err, inbox.ErrConflict):
		fmt.Fprintf(stderr, "hookinlet: an inbox named %q already exists\n", *slug)
		return exitRuntime
	case err != nil:
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}

	fmt.Fprintf(stdout, "created inbox %q\n", in.Slug)
	fmt.Fprintf(stdout, "  ingest path:  /in/%s\n", in.Slug)
	fmt.Fprintf(stdout, "  agent token:  %s\n", agentToken)
	fmt.Fprintln(stdout, "\nThe agent token is shown only once and cannot be recovered; store it now.")
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
