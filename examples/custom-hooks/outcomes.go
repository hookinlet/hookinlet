// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// OutcomeCounter counts how deliveries ended, per inbox and outcome, and serves the
// counts as plain text. It is an inbox.IngestObserver: the server finds that with a
// type assertion on the Hooks it is given, and then tells it how every delivery to
// a known inbox ended, refused ones included. The server's own /metrics counts the
// same outcomes, but only in total.
//
// It wraps the Hooks that do the rest (here the RateLimiter) by embedding the
// interface, so BeforeIngest and AfterIngest are theirs. Embedding an interface
// hides any other optional interface the wrapped value has (an Admitter, say); a
// wrapper that needs one passes it on with a method of its own.
//
// It keeps one number per inbox and outcome for as long as it runs, which is fine
// for an example and for a server with a bounded number of inboxes.
type OutcomeCounter struct {
	inbox.Hooks

	mu     sync.Mutex
	counts map[outcomeKey]int64
}

// outcomeKey names an inbox by tenant and slug: a slug is unique only within a
// tenant, so two tenants' inboxes called "orders" are counted apart.
type outcomeKey struct {
	tenant, slug string
	outcome      inbox.IngestOutcome
}

// NewOutcomeCounter counts outcomes and passes everything else to next.
func NewOutcomeCounter(next inbox.Hooks) *OutcomeCounter {
	return &OutcomeCounter{Hooks: next, counts: make(map[outcomeKey]int64)}
}

// ObserveIngest implements inbox.IngestObserver. It runs on every delivery's
// request, before the answer is sent, so it only takes a lock and adds one.
func (c *OutcomeCounter) ObserveIngest(_ context.Context, ev *inbox.IngestEvent) {
	k := outcomeKey{tenant: ev.Inbox.Tenant, slug: ev.Inbox.Slug, outcome: ev.Outcome}
	c.mu.Lock()
	c.counts[k]++
	c.mu.Unlock()
}

// ServeHTTP writes the counts, one line each, in the Prometheus text format:
//
//	example_deliveries_total{tenant="acme",inbox="orders",outcome="verification_failed"} 3
//
// It says which inboxes exist and how busy they are, so serve it only where the
// operator can reach it, as the stock binary does with its own /metrics.
func (c *OutcomeCounter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	lines := make([]string, 0, len(c.counts))
	for k, n := range c.counts {
		lines = append(lines, fmt.Sprintf("example_deliveries_total{tenant=%s,inbox=%s,outcome=%s} %d",
			label(k.tenant), label(k.slug), label(string(k.outcome)), n))
	}
	c.mu.Unlock()
	slices.Sort(lines)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintln(w, "# TYPE example_deliveries_total counter")
	for _, l := range lines {
		_, _ = fmt.Fprintln(w, l)
	}
}

// label quotes a label value as the Prometheus text format wants it.
func label(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}
