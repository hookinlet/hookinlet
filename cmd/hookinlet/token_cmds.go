// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
)

const accessUsage = `Usage: hookinlet token <command> [flags]

Commands:
  create    Issue a token for some inboxes (--inbox, repeatable) or all (--all-inboxes),
            optionally one that stops working after --expires-in or at --expires-at
  list      List tokens with their expiry and when each was last used; --inbox SLUG
            shows those that can read that inbox
  revoke ID Revoke a token; it stops working at once

An inbox can have any number of tokens, so a token is replaced without a gap:
create the new one, switch the agent over, then revoke the old one. A token is
shown once, when it is created.

With --url (or HOOKINLET_URL) these call a running server's admin API using the
admin token (HOOKINLET_ADMIN_TOKEN). Without it they work directly in the data
directory (--data-dir). Run "hookinlet token <command> -h" for a command's flags.
`

func tokenCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, accessUsage)
		return exitUsage
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, accessUsage)
		return 0
	case "create":
		return tokenCreate(rest, getenv, stdout, stderr)
	case "list":
		return tokenList(rest, getenv, stdout, stderr)
	case "revoke":
		return tokenRevoke(rest, getenv, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hookinlet: unknown token command %q\n\n%s", args[0], accessUsage)
		return exitUsage
	}
}

func tokenCreate(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet token create", flag.ContinueOnError)
	usageWith(fs, "hookinlet token create --name NAME (--inbox SLUG... | --all-inboxes) [flags]")
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	name := fs.String("name", "", "a name for people to tell tokens apart by (required)")
	all := fs.Bool("all-inboxes", false, "the token reads every inbox, those made later included")
	expiresIn := fs.String("expires-in", "", "the token stops working after this long, such as 720h or 90d (default: it never expires)")
	expiresAt := fs.String("expires-at", "", "the token stops working at this time, RFC 3339, such as 2027-01-31T00:00:00Z")
	var inboxes []string
	fs.Func("inbox", "slug of an inbox the token may read; repeat for more", func(v string) error {
		inboxes = append(inboxes, v)
		return nil
	})
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *name == "" {
		fmt.Fprintln(stderr, "hookinlet: --name is required")
		return exitUsage
	}
	if *all == (len(inboxes) > 0) {
		fmt.Fprintln(stderr, "hookinlet: give --inbox SLUG (once per inbox) or --all-inboxes, but not both and not neither")
		return exitUsage
	}
	expires, err := parseExpiry(*expiresIn, *expiresAt, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
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
	created, err := api.CreateToken(ctx, admin.TokenParams{Name: *name, Inboxes: inboxes, AllInboxes: *all, ExpiresAt: expires})
	if err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, created)
		return 0
	}
	fmt.Fprintf(stdout, "created token %d (%s) for %s\n", created.Record.ID, created.Record.Name, describeScope(created.Record))
	fmt.Fprintf(stdout, "  agent token:  %s\n", created.Token)
	if created.Record.ExpiresAt != nil {
		fmt.Fprintf(stdout, "  expires:      %s\n", *created.Record.ExpiresAt)
	}
	fmt.Fprintln(stdout, "\nThe token is shown only once and cannot be recovered; store it now.")
	return 0
}

// parseExpiry turns --expires-in (a duration; "d" counts days) or --expires-at
// (RFC 3339) into a time, or nil when neither is given. Giving both is an error, and
// so is a time that is not after now: the server would refuse it too, but this says why.
func parseExpiry(in, at string, now time.Time) (*time.Time, error) {
	switch {
	case in != "" && at != "":
		return nil, errors.New("give --expires-in or --expires-at, not both")
	case in != "":
		d, err := parseLife(in)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("--expires-in must be a positive duration such as 720h or 90d, not %q", in)
		}
		t := now.Add(d).UTC()
		return &t, nil
	case at != "":
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, fmt.Errorf("--expires-at must be a time like 2027-01-31T00:00:00Z, not %q", at)
		}
		if !t.After(now) {
			return nil, fmt.Errorf("--expires-at %s is not in the future", at)
		}
		return &t, nil
	}
	return nil, nil
}

// parseLife reads a Go duration, or a whole number of days such as "90d".
func parseLife(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(days, 10, 32)
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func describeScope(t admin.TokenView) string {
	if t.AllInboxes {
		return "every inbox"
	}
	return "inbox " + strings.Join(t.Inboxes, ", ")
}

func tokenList(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet token list", flag.ContinueOnError)
	usageWith(fs, "hookinlet token list [flags]")
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	slug := fs.String("inbox", "", "only the tokens that can read this inbox")
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
	views, err := api.ListTokens(ctx, *slug)
	if err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, map[string]any{"tokens": views})
		return 0
	}
	if len(views) == 0 {
		fmt.Fprintln(stdout, "no tokens")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tREADS\tCREATED\tEXPIRES\tLAST USED")
	now := time.Now()
	for _, v := range views {
		reads := "all inboxes"
		if !v.AllInboxes {
			reads = strings.Join(v.Inboxes, ", ")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Name, reads, v.CreatedAt, expiryColumn(v.ExpiresAt, now), orDash(v.LastUsedAt))
	}
	_ = tw.Flush()
	return 0
}

// expiryColumn is "never" for a token that does not expire, and marks one that has.
func expiryColumn(expires *string, now time.Time) string {
	if expires == nil {
		return "never"
	}
	if t, err := time.Parse(admin.TimeLayout, *expires); err == nil && !t.After(now) {
		return *expires + " (expired)"
	}
	return *expires
}

func orDash(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func tokenRevoke(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet token revoke", flag.ContinueOnError)
	usageWith(fs, "hookinlet token revoke ID [flags]")
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	// The id comes first, then any flags.
	var idArg string
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return exitUsage
	}
	if fs.NArg() > 0 {
		idArg = fs.Arg(0)
		if code, done := parseFlags(fs, fs.Args()[1:], stderr); done {
			return code
		}
	}
	id, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil || id < 1 {
		fmt.Fprintln(stderr, "hookinlet: the token ID is required, as shown by \"hookinlet token list\"")
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
	if err := api.RevokeToken(ctx, id); err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, map[string]any{"revoked": id})
		return 0
	}
	fmt.Fprintf(stdout, "revoked token %d; it no longer works\n", id)
	return 0
}
