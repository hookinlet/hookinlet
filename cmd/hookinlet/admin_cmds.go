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
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/config"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

const (
	urlEnv   = config.EnvPrefix + "URL"
	tokenEnv = config.EnvPrefix + "ADMIN_TOKEN"

	// callTimeout bounds every call but a backup, which takes as long as the
	// database is big.
	callTimeout = 60 * time.Second
)

// conn is the flags that say where the data is and how to reach it.
//
// With --url (or HOOKINLET_URL) the commands call a running server's admin API
// and need the admin token. Without it they work straight in the data
// directory, which needs no server and no token, only access to the files: this
// is for setting up before the first start, and for when the server cannot be
// reached. Giving both is an error, since it would not be clear which was meant.
type conn struct {
	url      *string
	token    *string
	insecure *bool
	asJSON   *bool
	data     func() (*config.DataSettings, error)
	getenv   func(string) string
	fs       *flag.FlagSet
}

func addConn(fs *flag.FlagSet, getenv func(string) string) *conn {
	return &conn{
		fs:       fs,
		getenv:   getenv,
		data:     config.AddDataFlags(fs, getenv),
		url:      fs.String("url", "", "address of a running server such as https://hooks.example.com; manage it through its admin API [env "+urlEnv+"]"),
		token:    fs.String("token", "", "admin token for --url (prefer the environment variable: flags are visible in process listings) [env "+tokenEnv+"]"),
		insecure: fs.Bool("insecure-http", false, "allow sending the admin token over plain http:// to another machine"),
		asJSON:   fs.Bool("json", false, "print the server's answer as JSON"),
	}
}

// open connects. It returns the API, a function that releases it, and an exit
// code that is non-zero when the caller should stop.
func (c *conn) open(ctx context.Context, stderr io.Writer, create bool) (adminAPI, func(), int) {
	data, err := c.data()
	if err != nil {
		return nil, nil, configError(stderr, err)
	}
	remote := *c.url
	if remote == "" {
		remote = c.getenv(urlEnv)
	}
	if remote != "" {
		if data.DataDirSet {
			fmt.Fprintln(stderr, "hookinlet: give --url or --data-dir, not both: one reaches a running server and the other opens the files directly")
			return nil, nil, exitUsage
		}
		tok := *c.token
		if tok == "" {
			tok = c.getenv(tokenEnv)
		}
		if tok == "" {
			fmt.Fprintf(stderr, "hookinlet: the admin token is required with --url: set %s (or pass --token)\n", tokenEnv)
			return nil, nil, exitUsage
		}
		api, err := newRemoteAdmin(remote, tok, *c.insecure)
		if err != nil {
			fmt.Fprintf(stderr, "hookinlet: %v\n", err)
			return nil, nil, exitUsage
		}
		return api, func() {}, 0
	}

	// Opening creates a database if there is none. Only `inbox create` should
	// do that: for anything else it means a mistyped --data-dir, and an empty
	// database that looks like a successful backup or an empty inbox list.
	if _, err := os.Stat(filepath.Join(data.DataDir, sqlite.DBFileName)); err != nil && !create {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "hookinlet: there is no database in %s; check --data-dir (or give --url to use a running server)\n", data.DataDir)
		} else {
			fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		}
		return nil, nil, exitRuntime
	}
	store, err := sqlite.Open(ctx, data.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return nil, nil, exitRuntime
	}
	svc := &admin.Service{Store: store}
	if data.MasterKey != "" {
		box, err := secretbox.New([]byte(data.MasterKey.Reveal()))
		if err != nil {
			_ = store.Close()
			fmt.Fprintf(stderr, "hookinlet: %v\n", err)
			return nil, nil, exitRuntime
		}
		svc.Box = box
	}
	return directAdmin{svc}, func() { _ = store.Close() }, 0
}

// parseSlugArgs parses flags and one positional SLUG, in either order.
func parseSlugArgs(fs *flag.FlagSet, args []string, stderr io.Writer) (slug string, code int, done bool) {
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return "", 0, true
	case err != nil:
		return "", exitUsage, true
	}
	if fs.NArg() > 0 {
		slug = fs.Arg(0)
		switch err := fs.Parse(fs.Args()[1:]); {
		case errors.Is(err, flag.ErrHelp):
			return "", 0, true
		case err != nil:
			return "", exitUsage, true
		}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hookinlet: unexpected argument %q\n", fs.Arg(0))
		return "", exitUsage, true
	}
	if slug == "" {
		fmt.Fprintln(stderr, "hookinlet: the inbox SLUG is required")
		return "", exitUsage, true
	}
	return slug, 0, false
}

// notifyContext is cancelled by Ctrl-C, so a long backup can be abandoned.
func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// fail reports an error from a call and returns the exit code.
func fail(stderr io.Writer, err error) int {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		fmt.Fprintf(stderr, "hookinlet: %s (HTTP %d)\n", ae.Message, ae.Status)
	case errors.Is(err, inbox.ErrInvalid):
		fmt.Fprintf(stderr, "hookinlet: %s\n", strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
	case errors.Is(err, inbox.ErrNotFound):
		fmt.Fprintln(stderr, "hookinlet: no such inbox")
	case errors.Is(err, inbox.ErrConflict) && strings.Contains(err.Error(), "ingest key"):
		fmt.Fprintln(stderr, "hookinlet: that ingest key is already in use by another inbox")
	case errors.Is(err, inbox.ErrConflict):
		fmt.Fprintln(stderr, "hookinlet: an inbox with that slug already exists, or it was changed at the same time")
	case errors.Is(err, admin.ErrBusy):
		fmt.Fprintln(stderr, "hookinlet: that is already running")
	default:
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
	}
	return exitRuntime
}

func printJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

const inboxUsage = `Usage: hookinlet inbox <command> [flags]

Commands:
  create        Create an inbox and print its agent token (once)
  list          List inboxes
  show SLUG     Show one inbox
  update SLUG   Change an inbox's settings
  set-secret SLUG    Set or replace the verifier secret
  rotate-token SLUG  Issue a new agent token; the old one stops working
  rotate-ingest-key SLUG  Give the inbox a new ingest URL; the old one stops working
  delete SLUG   Delete an inbox and all of its messages

With --url (or HOOKINLET_URL) these call a running server's admin API using the
admin token (HOOKINLET_ADMIN_TOKEN). Without it they work directly in the data
directory (--data-dir). Run "hookinlet inbox <command> -h" for a command's flags.
`

func inboxCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, inboxUsage)
		return exitUsage
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, inboxUsage)
		return 0
	case "create":
		return inboxCreate(rest, getenv, stdout, stderr)
	case "list":
		return inboxList(rest, getenv, stdout, stderr)
	case "show":
		return inboxShow(rest, getenv, stdout, stderr)
	case "update":
		return inboxUpdate(rest, getenv, stdout, stderr)
	case "set-secret":
		return inboxSetSecret(rest, getenv, stdin, stdout, stderr)
	case "rotate-token":
		return inboxRotate(rest, getenv, stdout, stderr)
	case "rotate-ingest-key":
		return inboxRotateIngestKey(rest, getenv, stdout, stderr)
	case "delete":
		return inboxDelete(rest, getenv, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hookinlet: unknown inbox command %q\n\n%s", args[0], inboxUsage)
		return exitUsage
	}
}

// stdin is replaced in tests.
var stdin io.Reader = os.Stdin

func inboxCreate(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	var (
		slug       = fs.String("slug", "", "inbox slug: 3-64 characters from a-z, 0-9, and hyphen (required)")
		name       = fs.String("name", "", "display name (default: the slug)")
		verifier   = fs.String("verifier", "", "how deliveries are checked: stripe, github, slack, standardwebhooks, hmac, token, or none for no check (required)")
		ingestKey  = fs.String("ingest-key", "", "the key in the ingest URL; leave it out to get a random one, which is what an inbox should have")
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
	if *slug == "" {
		fmt.Fprintln(stderr, "hookinlet: --slug is required")
		return exitUsage
	}
	if *verifier == "" {
		fmt.Fprintln(stderr, "hookinlet: --verifier is required: say how deliveries are checked (stripe, github, slack, standardwebhooks, hmac, token), or none to accept anything unchecked")
		return exitUsage
	}
	if !json.Valid([]byte(*vconfig)) {
		fmt.Fprintln(stderr, "hookinlet: --verifier-config is not valid JSON")
		return exitUsage
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, true)
	if code != 0 {
		return code
	}
	defer closeAPI()

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	created, err := api.Create(ctx, admin.CreateParams{
		Slug: *slug, IngestKey: *ingestKey, Name: *name, Verifier: *verifier, VerifierConfig: json.RawMessage(*vconfig),
		VerifierSecret: getenv(verifierSecretEnv), OnVerifyFail: *onFail, MaxBodyBytes: *maxBody,
		RetentionDays: *retDays, RetentionMaxCount: *retCount, EventTypePointer: *eventPoint,
	})
	switch {
	case errors.Is(err, admin.ErrNoSecretBox):
		fmt.Fprintf(stderr, "hookinlet: %s is set, so --master-key (%s) is required to seal it\n", verifierSecretEnv, config.EnvPrefix+"MASTER_KEY")
		return exitUsage
	case err != nil:
		return failCreate(stderr, err, *slug)
	}
	if *c.asJSON {
		printJSON(stdout, created)
		return 0
	}
	printCreated(stdout, "created inbox", created)
	return 0
}

// failCreate is fail with the slug named for a conflict, since creating is the
// one command where "already exists" is the likely reason.
func failCreate(stderr io.Writer, err error, slug string) int {
	var ae *apiError
	if errors.Is(err, inbox.ErrConflict) || (errors.As(err, &ae) && ae.Status == 409) {
		if strings.Contains(err.Error(), "ingest key") || (ae != nil && strings.Contains(ae.Message, "ingest key")) {
			fmt.Fprintln(stderr, "hookinlet: that ingest key is already in use by another inbox")
			return exitRuntime
		}
		fmt.Fprintf(stderr, "hookinlet: an inbox named %q already exists\n", slug)
		return exitRuntime
	}
	if errors.Is(err, inbox.ErrInvalid) {
		fmt.Fprintf(stderr, "hookinlet: %s\n", strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
		return exitUsage
	}
	if errors.As(err, &ae) && ae.Status == 400 {
		fmt.Fprintf(stderr, "hookinlet: %s\n", ae.Message)
		return exitUsage
	}
	return fail(stderr, err)
}

// failUpdate is fail with a bad setting reported as a usage error, and a
// conflict explained for what it is when updating: someone else changed the
// inbox at the same moment.
func failUpdate(stderr io.Writer, err error) int {
	var ae *apiError
	switch {
	case errors.Is(err, inbox.ErrConflict) || (errors.As(err, &ae) && ae.Status == 409):
		fmt.Fprintln(stderr, "hookinlet: the inbox was changed by someone else at the same moment; run the command again")
		return exitRuntime
	case errors.Is(err, inbox.ErrInvalid):
		fmt.Fprintf(stderr, "hookinlet: %s\n", strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
		return exitUsage
	case errors.As(err, &ae) && ae.Status == 400:
		fmt.Fprintf(stderr, "hookinlet: %s\n", ae.Message)
		return exitUsage
	}
	return fail(stderr, err)
}

func printCreated(w io.Writer, verb string, c *admin.Created) {
	fmt.Fprintf(w, "%s %q\n", verb, c.Inbox.Slug)
	fmt.Fprintf(w, "  ingest path:  %s\n", c.Inbox.IngestPath)
	if c.Inbox.IngestURL != "" {
		fmt.Fprintf(w, "  ingest url:   %s\n", c.Inbox.IngestURL)
	}
	fmt.Fprintf(w, "  agent token:  %s\n", c.AgentToken)
	for _, warn := range c.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}
	fmt.Fprintln(w, "\nThe agent token is shown only once and cannot be recovered; store it now.")
}

func inboxList(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	views, err := api.List(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, map[string]any{"inboxes": views})
		return 0
	}
	if len(views) == 0 {
		fmt.Fprintln(stdout, "no inboxes")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tNAME\tVERIFIER\tSECRET\tLAST SEQ\tSTATE")
	for _, v := range views {
		state := "active"
		if v.Disabled {
			state = "disabled"
		}
		secret := "-"
		if v.HasVerifierSecret {
			secret = "set"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", v.Slug, v.Name, v.Verifier, secret, v.LastSeq, state)
	}
	_ = tw.Flush()
	return 0
}

func inboxShow(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	slug, code, done := parseSlugArgs(fs, args, stderr)
	if done {
		return code
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	v, err := api.Get(ctx, slug)
	if err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, v)
		return 0
	}
	printView(stdout, v)
	return 0
}

func printView(w io.Writer, v admin.View) {
	secret := "no secret set"
	if v.HasVerifierSecret {
		secret = "secret set"
	}
	state := "active"
	if v.Disabled {
		state = "disabled (deliveries get 404; agents can still read)"
	}
	row := func(k, val string) { fmt.Fprintf(w, "%-20s %s\n", k+":", val) }
	row("slug", v.Slug)
	row("ingest key", v.IngestKey)
	row("name", v.Name)
	row("state", state)
	row("verifier", fmt.Sprintf("%s (%s)", v.Verifier, secret))
	row("verifier config", string(v.VerifierConfig))
	row("on verify fail", v.OnVerifyFail)
	row("max body bytes", fmt.Sprint(v.MaxBodyBytes))
	row("retention", retentionText(v))
	if v.EventTypePointer != "" {
		row("event type pointer", v.EventTypePointer)
	}
	count := ""
	if v.MessageCount != nil {
		count = fmt.Sprintf("%d messages, ", *v.MessageCount)
	}
	row("messages", fmt.Sprintf("%slatest seq %d", count, v.LastSeq))
	row("ingest path", v.IngestPath)
	if v.IngestURL != "" {
		row("ingest url", v.IngestURL)
	}
	row("created", v.CreatedAt)
	row("updated", fmt.Sprintf("%s (version %d)", v.UpdatedAt, v.Version))
}

func retentionText(v admin.View) string {
	var parts []string
	if v.RetentionDays > 0 {
		parts = append(parts, fmt.Sprintf("older than %d days", v.RetentionDays))
	}
	if v.RetentionMaxCount > 0 {
		parts = append(parts, fmt.Sprintf("beyond the newest %d", v.RetentionMaxCount))
	}
	if len(parts) == 0 {
		return "keep everything"
	}
	return "delete messages " + strings.Join(parts, " or ")
}

func inboxUpdate(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	var (
		name       = fs.String("name", "", "display name")
		verifier   = fs.String("verifier", "", "none, token, hmac, stripe, github, slack, or standardwebhooks")
		vconfig    = fs.String("verifier-config", "", "verifier parameters as a JSON object")
		onFail     = fs.String("on-verify-fail", "", "reject or store")
		maxBody    = fs.Int64("max-body-bytes", 0, "largest accepted body")
		retDays    = fs.Int("retention-days", 0, "delete messages older than this many days; 0 keeps them")
		retCount   = fs.Int64("retention-max-count", 0, "keep at most this many newest messages; 0 means no limit")
		eventPoint = fs.String("event-type-pointer", "", "JSON pointer to the event type; empty clears it")
		disabled   = fs.Bool("disabled", false, "stop accepting deliveries (--disabled=false to resume)")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: hookinlet inbox update SLUG [flags]\n\nOnly the settings you give are changed.\n\n")
		fs.PrintDefaults()
	}
	slug, code, done := parseSlugArgs(fs, args, stderr)
	if done {
		return code
	}
	var p admin.Patch
	bad := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			p.Name = name
		case "verifier":
			p.Verifier = verifier
		case "verifier-config":
			if !json.Valid([]byte(*vconfig)) {
				fmt.Fprintln(stderr, "hookinlet: --verifier-config is not valid JSON")
				bad = true
				return
			}
			raw := json.RawMessage(*vconfig)
			p.VerifierConfig = &raw
		case "on-verify-fail":
			p.OnVerifyFail = onFail
		case "max-body-bytes":
			p.MaxBodyBytes = maxBody
		case "retention-days":
			p.RetentionDays = retDays
		case "retention-max-count":
			p.RetentionMaxCount = retCount
		case "event-type-pointer":
			p.EventTypePointer = eventPoint
		case "disabled":
			p.Disabled = disabled
		}
	})
	if bad {
		return exitUsage
	}
	if p == (admin.Patch{}) {
		fmt.Fprintln(stderr, "hookinlet: nothing to change; give at least one setting such as --name")
		return exitUsage
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	v, err := api.Update(ctx, slug, p)
	if err != nil {
		return failUpdate(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, v)
		return 0
	}
	fmt.Fprintf(stdout, "updated inbox %q (now version %d)\n", v.Slug, v.Version)
	for _, w := range v.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return 0
}

func inboxSetSecret(args []string, getenv func(string) string, in io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox set-secret", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	fromStdin := fs.Bool("secret-stdin", false, "read the secret from standard input instead of the environment")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: hookinlet inbox set-secret SLUG [flags]\n\nThe secret is read from %s, or from standard input with --secret-stdin.\nIt is never accepted as a flag, because flags are visible in process listings.\n\n", verifierSecretEnv)
		fs.PrintDefaults()
	}
	slug, code, done := parseSlugArgs(fs, args, stderr)
	if done {
		return code
	}
	secret := getenv(verifierSecretEnv)
	if *fromStdin {
		b, err := io.ReadAll(io.LimitReader(in, admin.MaxSecretBytes+2))
		if err != nil {
			fmt.Fprintf(stderr, "hookinlet: reading the secret: %v\n", err)
			return exitRuntime
		}
		secret = strings.TrimRight(string(b), "\r\n")
	}
	if secret == "" {
		fmt.Fprintf(stderr, "hookinlet: no secret given: set %s or use --secret-stdin\n", verifierSecretEnv)
		return exitUsage
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	v, err := api.SetSecret(ctx, slug, secret)
	switch {
	case errors.Is(err, admin.ErrNoSecretBox):
		fmt.Fprintf(stderr, "hookinlet: --master-key (%s) is required to seal the secret\n", config.EnvPrefix+"MASTER_KEY")
		return exitUsage
	case err != nil:
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, v)
		return 0
	}
	fmt.Fprintf(stdout, "verifier secret set for inbox %q\n", v.Slug)
	for _, w := range v.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return 0
}

func inboxRotate(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox rotate-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	slug, code, done := parseSlugArgs(fs, args, stderr)
	if done {
		return code
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	created, err := api.RotateToken(ctx, slug)
	if err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, created)
		return 0
	}
	fmt.Fprintf(stdout, "rotated the agent token of inbox %q; the old one no longer works\n", created.Inbox.Slug)
	fmt.Fprintf(stdout, "  agent token:  %s\n", created.AgentToken)
	fmt.Fprintln(stdout, "\nThe agent token is shown only once and cannot be recovered; store it now.")
	return 0
}

func inboxRotateIngestKey(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox rotate-ingest-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	slug, code, done := parseSlugArgs(fs, args, stderr)
	if done {
		return code
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	v, err := api.RotateIngestKey(ctx, slug)
	if err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, v)
		return 0
	}
	fmt.Fprintf(stdout, "gave inbox %q a new ingest key; the old one no longer works\n", v.Slug)
	fmt.Fprintf(stdout, "  ingest path:  %s\n", v.IngestPath)
	fmt.Fprintln(stdout, "\nGive senders the new URL.")
	return 0
}

func inboxDelete(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet inbox delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	yes := fs.Bool("yes", false, "confirm: delete the inbox and every message in it")
	slug, code, done := parseSlugArgs(fs, args, stderr)
	if done {
		return code
	}
	if !*yes {
		fmt.Fprintf(stderr, "hookinlet: this deletes inbox %q and every message in it, and cannot be undone; pass --yes to go ahead\n", slug)
		return exitUsage
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := api.Delete(ctx, slug); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "deleted inbox %q\n", slug)
	return 0
}

// ---- retention and backup ----

func retentionCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(stderr, "Usage: hookinlet retention run [flags]\n\nApply every inbox's retention policy now.\n")
		if len(args) == 0 {
			return exitUsage
		}
		if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
			return 0
		}
		fmt.Fprintf(stderr, "hookinlet: unknown retention command %q\n", args[0])
		return exitUsage
	}
	fs := flag.NewFlagSet("hookinlet retention run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	if code, done := parseFlags(fs, args[1:], stderr); done {
		return code
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	n, err := api.RunRetention(ctx) // a big prune can take minutes; Ctrl-C stops it
	if err != nil {
		if n > 0 {
			fmt.Fprintf(stderr, "hookinlet: stopped after deleting %d messages\n", n)
		}
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, map[string]int64{"deleted": n})
		return 0
	}
	fmt.Fprintf(stdout, "deleted %d messages\n", n)
	return 0
}

func backupCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	out := fs.String("out", "", "file to write the backup to (required)")
	force := fs.Bool("force", false, "replace the file if it exists")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: hookinlet backup --out FILE [flags]

Write a consistent copy of the whole database: every inbox, message, and token
hash, and the verifier secrets still sealed. To use it, put it at
<data-dir>/hookinlet.db with the same master key. Protect the file like the
data directory itself.

`)
		fs.PrintDefaults()
	}
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *out == "" {
		fmt.Fprintln(stderr, "hookinlet: --out is required")
		return exitUsage
	}
	// Say so before downloading, not after.
	if !*force {
		if _, err := os.Lstat(*out); err == nil {
			fmt.Fprintf(stderr, "hookinlet: %s already exists; pass --force to replace it\n", *out)
			return exitRuntime
		}
	}
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()

	snap, size, err := api.Backup(ctx)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 501 || errors.Is(err, admin.ErrNoBackup) {
			fmt.Fprintln(stderr, "hookinlet: this server's store does not support backups; back up the database with the tools of whatever hosts it")
			return exitRuntime
		}
		return fail(stderr, err)
	}
	defer func() { _ = snap.Close() }()

	// Write beside the destination and rename, so an interrupted backup never
	// leaves something that looks like a whole one.
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".hookinlet-backup-*")
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	n, err := io.Copy(tmp, snap)
	if err == nil {
		err = tmp.Sync() // so a power cut cannot leave a complete-looking file with missing data
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "hookinlet: the backup was interrupted after %d of %d bytes: %v\n", n, size, err)
		return exitRuntime
	case n != size:
		fmt.Fprintf(stderr, "hookinlet: the backup is incomplete: %d of %d bytes\n", n, size)
		return exitRuntime
	}
	if err := placeBackup(tmpName, *out, *force); err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitRuntime
	}
	if *c.asJSON {
		printJSON(stdout, map[string]any{"file": *out, "bytes": n})
		return 0
	}
	fmt.Fprintf(stdout, "wrote %d bytes to %s\n", n, *out)
	return 0
}

// placeBackup moves the finished file to its destination. Without force it must
// not replace anything, including a file created since the check at the start,
// so it makes a hard link, which fails if the name exists, rather than renaming.
// Where links are not available it falls back to a check and a rename.
func placeBackup(tmp, out string, force bool) error {
	if force {
		return os.Rename(tmp, out)
	}
	err := os.Link(tmp, out)
	switch {
	case err == nil:
		return nil // the temporary name is removed by the caller
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s already exists; pass --force to replace it", out)
	}
	if _, statErr := os.Lstat(out); statErr == nil {
		return fmt.Errorf("%s already exists; pass --force to replace it", out)
	}
	return os.Rename(tmp, out)
}
