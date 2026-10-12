// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hookinlet/hookinlet/internal/config"
)

const healthcheckUsage = `Usage: hookinlet healthcheck [--url URL] [--timeout D] [--live]

Asks a running server whether it is ready (GET /readyz) and exits 0 if it is,
1 if it is not or cannot be reached. It is for a container's HEALTHCHECK, where
the image has no curl. With no --url it asks the address in HOOKINLET_LISTEN_ADDR
(default :8080) on this machine. --live asks /healthz instead, which says only
that the process answers.
`

// healthcheckCmd is the container's health probe. It sends no credential and
// asks for nothing but the status.
func healthcheckCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hookinlet healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, healthcheckUsage)
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	base := fs.String("url", "", "address of the server, such as http://127.0.0.1:8080 (default: this machine, at "+config.EnvPrefix+"LISTEN_ADDR)")
	timeout := fs.Duration("timeout", 3*time.Second, "how long to wait for an answer")
	live := fs.Bool("live", false, "check /healthz (the process answers) and not /readyz (it is ready for work)")
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "hookinlet: --timeout must be greater than zero")
		return exitUsage
	}
	target := *base
	if target == "" {
		target = localURL(getenv(config.EnvPrefix + "LISTEN_ADDR"))
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintf(stderr, "hookinlet: --url must be an http:// or https:// address, got %q\n", target)
		return exitUsage
	}
	path := "/readyz"
	if *live {
		path = "/healthz"
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target, "/")+path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: %v\n", err)
		return exitUsage
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "hookinlet: not healthy: %v\n", err)
		return exitRuntime
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "hookinlet: not healthy: %s answered %s\n", path, resp.Status)
		return exitRuntime
	}
	fmt.Fprintf(stdout, "ok %s\n", path)
	return 0
}

// localURL turns a listen address such as ":8080" or "0.0.0.0:8080" into one that
// reaches it from this machine.
func localURL(listen string) string {
	if listen == "" {
		listen = ":8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
