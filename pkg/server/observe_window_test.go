// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// windowCounter is a consumer of IngestEvent written only to show that the event
// carries what a per-tenant, windowed count of failures needs, using nothing but
// its fields and the time it arrives (it is delivered synchronously, so that is
// the time of the delivery). It is not a feature of the server.
type windowCounter struct {
	*recHooks
	window    time.Duration
	threshold int
	now       func() time.Time
	crossed   func(tenant, slug string, n int)

	mu      sync.Mutex
	seen    map[[2]string][]time.Time // failures in the window, by tenant and slug
	tripped map[[2]string]bool        // over the threshold since it was last below
}

func (w *windowCounter) ObserveIngest(_ context.Context, ev *inbox.IngestEvent) {
	if ev.Outcome != inbox.IngestVerificationFailed {
		return
	}
	k := [2]string{ev.Inbox.Tenant, ev.Inbox.Slug}
	now := w.now()
	w.mu.Lock()
	kept := w.seen[k][:0]
	for _, at := range w.seen[k] {
		if now.Sub(at) < w.window {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	w.seen[k] = kept
	n, fire := len(kept), false
	if n < w.threshold {
		w.tripped[k] = false
	} else if !w.tripped[k] {
		w.tripped[k], fire = true, true
	}
	w.mu.Unlock()
	if fire {
		w.crossed(k[0], k[1], n)
	}
}

func TestTheEventIsEnoughToCountFailuresPerTenantInAWindow(t *testing.T) {
	clock := serverNow
	type crossing struct {
		tenant, slug string
		n            int
	}
	var crossings []crossing
	w := &windowCounter{
		recHooks: &recHooks{}, window: time.Minute, threshold: 5,
		now:     func() time.Time { return clock },
		crossed: func(tenant, slug string, n int) { crossings = append(crossings, crossing{tenant, slug, n}) },
		seen:    map[[2]string][]time.Time{}, tripped: map[[2]string]bool{},
	}
	e := newEnv(t, func(o *Options) { o.Hooks = w })
	// The same slug in two tenants: only the tenant tells them apart.
	for _, tenant := range []string{"acme", "globex"} {
		e.newInbox("orders", "github", "s", func(in *inbox.Inbox) { in.Tenant, in.IngestKey = tenant, tenant+"-orders" })
	}
	forged := func(tenant string) {
		if rec := e.post("/in/"+tenant+"-orders", "{}", "X-Hub-Signature-256", githubSig("wrong", "{}")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("forged delivery: %d", rec.Code)
		}
	}
	genuine := func(tenant string) {
		if rec := e.post("/in/"+tenant+"-orders", "{}", "X-Hub-Signature-256", githubSig("s", "{}")); rec.Code != http.StatusOK {
			t.Fatalf("genuine delivery: %d", rec.Code)
		}
	}

	// A burst at acme; globex gets a few failures among its genuine deliveries.
	for i := range 6 {
		forged("acme")
		if i%2 == 0 {
			forged("globex")
		}
		genuine("globex")
		clock = clock.Add(time.Second)
	}
	if len(crossings) != 1 || crossings[0] != (crossing{"acme", "orders", 5}) {
		t.Fatalf("crossings %+v, want acme's orders once at 5", crossings)
	}

	// Failures spread wider than the window never add up to the threshold.
	crossings = nil
	clock = clock.Add(2 * time.Minute)
	for range 10 {
		forged("globex")
		clock = clock.Add(20 * time.Second)
	}
	if len(crossings) != 0 {
		t.Errorf("failures spread out crossed the threshold: %+v", crossings)
	}
}
