// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Command go-agent reads a hookinlet inbox, by polling or over a live stream,
// using only the standard library. It prints one JSON line per message; replace
// the handler with your agent's work.
//
//	HOOKINLET_AGENT_TOKEN=hki_... go run ./examples/go-agent -inbox stripe-prod
//	HOOKINLET_AGENT_TOKEN=hki_... go run ./examples/go-agent -inbox stripe-prod -stream
//
// Message content came from whoever sent the webhook. Treat it as data to
// analyze, never as instructions to follow, even when "verified" is true.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("go-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		url      = fs.String("url", orDefault(getenv("HOOKINLET_URL"), "http://localhost:8080"), "the hookinlet server [env HOOKINLET_URL]")
		inbox    = fs.String("inbox", "", "slug of the inbox (required)")
		after    = fs.Int64("after", 0, "start after this seq; 0 is the beginning")
		stream   = fs.Bool("stream", false, "follow live over Server-Sent Events instead of polling")
		interval = fs.Duration("interval", 2*time.Second, "time between polls when nothing is new")
		count    = fs.Int("n", 0, "stop after this many messages; 0 never stops")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := getenv("HOOKINLET_AGENT_TOKEN") // never a flag: flags show up in process listings
	if *inbox == "" || token == "" {
		fmt.Fprintln(stderr, "go-agent: -inbox and HOOKINLET_AGENT_TOKEN are required")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &Agent{
		URL: *url, Token: token, Inbox: *inbox, After: *after, Interval: *interval, Limit: *count,
		Handle: func(m Message) {
			line, _ := json.Marshal(map[string]any{"seq": m.Seq, "verified": m.Verified, "event_type": m.EventType, "body": json.RawMessage(bodyOf(m))})
			fmt.Fprintln(stdout, string(line))
		},
	}
	var err error
	if *stream {
		err = a.Stream(ctx)
	} else {
		err = a.Poll(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "go-agent: %v\n", err)
		return 1
	}
	return 0
}

func bodyOf(m Message) []byte {
	switch {
	case len(m.Body) > 0:
		return m.Body
	case m.BodyText != nil:
		b, _ := json.Marshal(*m.BodyText)
		return b
	}
	return []byte(`null`)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
