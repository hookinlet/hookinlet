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

	"github.com/hookinlet/hookinlet/internal/admin"
)

const accessUsage = `Usage: hookinlet token <command> [flags]

Commands:
  create    Issue a token for some inboxes (--inbox, repeatable) or all (--all-inboxes)
  list      List tokens; --inbox SLUG shows those that can read that inbox
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
	usageWith(fs, "hookinlet token create --name NAME [flags]")
	fs.SetOutput(stderr)
	c := addConn(fs, getenv)
	name := fs.String("name", "", "a name for people to tell tokens apart by (required)")
	all := fs.Bool("all-inboxes", false, "the token reads every inbox, those made later included")
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
	ctx, stop := notifyContext()
	defer stop()
	api, closeAPI, code := c.open(ctx, stderr, false)
	if code != 0 {
		return code
	}
	defer closeAPI()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	created, err := api.CreateToken(ctx, admin.TokenParams{Name: *name, Inboxes: inboxes, AllInboxes: *all})
	if err != nil {
		return fail(stderr, err)
	}
	if *c.asJSON {
		printJSON(stdout, created)
		return 0
	}
	fmt.Fprintf(stdout, "created token %d (%s) for %s\n", created.Record.ID, created.Record.Name, describeScope(created.Record))
	fmt.Fprintf(stdout, "  agent token:  %s\n", created.Token)
	fmt.Fprintln(stdout, "\nThe token is shown only once and cannot be recovered; store it now.")
	return 0
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
	fmt.Fprintln(tw, "ID\tNAME\tREADS\tCREATED")
	for _, v := range views {
		reads := "all inboxes"
		if !v.AllInboxes {
			reads = strings.Join(v.Inboxes, ", ")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", v.ID, v.Name, reads, v.CreatedAt)
	}
	_ = tw.Flush()
	return 0
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
