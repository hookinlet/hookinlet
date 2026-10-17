// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func counted(t *testing.T, c *OutcomeCounter) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	return rec.Body.String()
}

// Two tenants' inboxes with the same slug are counted apart, each by outcome.
func TestOutcomesAreCountedPerInboxAndTenant(t *testing.T) {
	c := NewOutcomeCounter(inbox.NopHooks{})
	acme := &inbox.Inbox{ID: 1, Slug: "orders", Tenant: "acme"}
	globex := &inbox.Inbox{ID: 2, Slug: "orders", Tenant: "globex"}
	for range 3 {
		c.ObserveIngest(t.Context(), &inbox.IngestEvent{Inbox: acme, Outcome: inbox.IngestVerificationFailed, Status: 401})
	}
	c.ObserveIngest(t.Context(), &inbox.IngestEvent{Inbox: acme, Outcome: inbox.IngestStored, Status: 200, Seq: 1})
	c.ObserveIngest(t.Context(), &inbox.IngestEvent{Inbox: globex, Outcome: inbox.IngestStored, Status: 200, Seq: 1})

	got := counted(t, c)
	for _, want := range []string{
		`example_deliveries_total{tenant="acme",inbox="orders",outcome="verification_failed"} 3`,
		`example_deliveries_total{tenant="acme",inbox="orders",outcome="stored"} 1`,
		`example_deliveries_total{tenant="globex",inbox="orders",outcome="stored"} 1`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, `tenant="globex",inbox="orders",outcome="verification_failed"`) {
		t.Errorf("one tenant's failures were counted against the other:\n%s", got)
	}
}

// A tenant key is any string; it cannot break a line or a label.
func TestLabelsAreQuoted(t *testing.T) {
	c := NewOutcomeCounter(inbox.NopHooks{})
	c.ObserveIngest(t.Context(), &inbox.IngestEvent{Inbox: &inbox.Inbox{Slug: "s", Tenant: "a\"b\\c\nd"}, Outcome: inbox.IngestStored})
	if got := counted(t, c); !strings.Contains(got, `tenant="a\"b\\c\nd"`) || strings.Count(got, "\n") != 2 {
		t.Errorf("got:\n%s", got)
	}
}

// The counter passes BeforeIngest to the hooks it wraps.
func TestTheCounterKeepsTheWrappedHooks(t *testing.T) {
	r, _ := limiter(1, 1)
	c := NewOutcomeCounter(r)
	in := &inbox.Inbox{ID: 1}
	_ = c.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in})
	if err := c.BeforeIngest(t.Context(), &inbox.IngestRequest{Inbox: in}); err == nil {
		t.Error("the wrapped rate limiter was not asked")
	}
}
