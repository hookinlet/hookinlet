// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Pat Ramsey

// Command relay forwards every message that arrives in a hookinlet inbox to
// another URL, so an inbox can sit in front of a service that already takes
// webhooks, instead of replacing it.
//
// It is a plain client of the query API, not part of the server: hookinlet keeps
// no delivery queue, and this program shows how little it takes to build one on
// the read API. It delivers at least once and in order, retries a destination
// that is down, and remembers where it got to in a file. The destination can
// de-duplicate with the X-Hookinlet-Seq header, which is unique per inbox.
//
//	HOOKINLET_URL=http://localhost:8080 \
//	HOOKINLET_AGENT_TOKEN=hki_... \
//	relay -inbox stripe-prod -to https://billing.internal/webhooks/stripe -state relay.state
//
// What the destination receives is the body exactly as the sender posted it, with
// the sender's headers (so it can verify the original signature, with the
// original secret) and some X-Hookinlet-* headers describing the message. Headers
// that say where a request came from (X-Forwarded-For and the like) and any
// X-Hookinlet-* the sender made up are not passed on, but every other header is
// the sender's, so the destination must not take one on trust.
// Messages that were not verified (they failed verification, or arrived at an
// inbox whose verifier is "none") are not forwarded unless -unverified is set.
//
// A message is retried until the destination takes it, whatever it answers, except
// that 400, 410, 413, 415, and 422 mean it refused this message for good, and it is
// skipped with a log line. A wrong address or an expired credential (a redirect,
// 401, 403, 404) is retried, so that nothing is lost while it is fixed. Senders that
// sign a timestamp (Stripe, Slack, Standard Webhooks) are checked by their receivers
// against a five minute window, so a message delivered after a long outage may be
// refused by the destination's own check.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

func run(args []string, getenv func(string) string, stderr *os.File) int {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		source     = fs.String("url", orDefault(getenv("HOOKINLET_URL"), "http://localhost:8080"), "address of the hookinlet server [env HOOKINLET_URL]")
		inbox      = fs.String("inbox", "", "slug of the inbox to relay (required)")
		to         = fs.String("to", "", "URL to forward each message to (required)")
		state      = fs.String("state", "", "file that remembers the position, so a restart carries on (required)")
		start      = fs.String("start", "now", "where to begin when there is no state file: now, or beginning")
		unverified = fs.Bool("unverified", false, "also forward messages that were not verified")
		poll       = fs.Duration("poll", time.Second, "how often to look for new messages when idle")
		maxBackoff = fs.Duration("max-backoff", 30*time.Second, "longest wait between attempts to deliver one message")
		strip      = fs.String("strip", "", "comma-separated headers not to pass on, besides X-Forwarded-*, Forwarded, X-Real-Ip, Via, and X-Hookinlet-*")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := getenv("HOOKINLET_AGENT_TOKEN") // never a flag: flags show up in process listings
	switch {
	case *source == "" || *inbox == "" || *to == "" || *state == "":
		fmt.Fprintln(stderr, "relay: -inbox, -to, and -state are required")
		return 2
	case token == "":
		fmt.Fprintln(stderr, "relay: set HOOKINLET_AGENT_TOKEN to the inbox's agent token")
		return 2
	case *start != "now" && *start != "beginning":
		fmt.Fprintln(stderr, "relay: -start must be now or beginning")
		return 2
	}
	for _, u := range []string{*source, *to} {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			fmt.Fprintf(stderr, "relay: %q is not an http:// or https:// address\n", u)
			return 2
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &Relay{
		Source: *source, Token: token, Inbox: *inbox, Destination: *to, StateFile: *state, Start: *start,
		IncludeUnverified: *unverified, PollInterval: *poll, MaxBackoff: *maxBackoff, StripHeaders: strings.Split(*strip, ","),
		Log: slog.New(slog.NewTextHandler(stderr, nil)),
	}
	if err := r.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "relay: %v\n", err)
		return 1
	}
	return 0
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
