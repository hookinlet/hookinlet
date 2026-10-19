// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package servertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// secretInboxes makes, for each tenant in order, an inbox with a sealed verifier
// secret, and marks the secrets of the tenants in failing as unable to open. The
// secrets are "plain-<slug>", which no log may show.
func secretInboxes(tenants []string, failing ...string) config {
	return config{extra: func(t *testing.T, s inbox.Store, box *failingBox) {
		t.Helper()
		for i, tenant := range tenants {
			slug := fmt.Sprintf("hook-%03d", i)
			uid, err := inbox.NewUID()
			if err != nil {
				t.Fatal(err)
			}
			sealed, err := inbox.SealWith(inbox.WithTenant(t.Context(), tenant), box, []byte("plain-"+slug), inbox.VerifierSecretContext(uid))
			if err != nil {
				t.Fatal(err)
			}
			in := &inbox.Inbox{
				Slug: slug, UID: uid, IngestKey: "key-" + slug, Name: slug, Verifier: "github",
				VerifierSecret: sealed, Tenant: tenant,
			}
			if err := s.CreateInbox(t.Context(), in); err != nil {
				t.Fatalf("creating %s/%s: %v", tenant, slug, err)
			}
			for _, f := range failing {
				if f == tenant {
					box.setDown(inbox.VerifierSecretContext(uid))
				}
			}
		}
	}}
}

func repeat(tenant string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = tenant
	}
	return out
}

// readyz asks /readyz and returns the status and body.
func (f *fixture) readyz() (int, string) {
	f.t.Helper()
	return f.get("/readyz", nil)
}

// secretLines returns the log lines about secrets that do not open, by the tenant they
// name ("" for a line without a tenant field).
func secretLines(f *fixture) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if _, ok := m["unreadable"]; !ok {
			continue
		}
		tenant, _ := m["tenant"].(string)
		out[tenant]++
	}
	return out
}

// readySampleMax is the most secrets /readyz opens (docs/extending.md).
const readySampleMax = 25

func runReadiness(t *testing.T, h Harness) {
	// One tenant whose key is gone does not take the server, and every other tenant on
	// it, out of rotation, even when that tenant owns the oldest inboxes. It is named in
	// the log.
	t.Run("OneTenantsSecretsDoNotOpen", func(t *testing.T) {
		f := newFixture(t, h, secretInboxes(append(repeat("org-a", 30), repeat("org-b", 3)...), "org-a"))
		if status, body := f.readyz(); status != http.StatusOK {
			t.Fatalf("/readyz with one tenant's key gone: %d %s; want 200", status, body)
		}
		if n := f.box.opens.Load(); n > readySampleMax {
			t.Errorf("readiness opened %d secrets, more than its sample of %d", n, readySampleMax)
		}
		if lines := secretLines(f); lines["org-a"] != 1 || len(lines) != 1 {
			t.Errorf("log lines about unreadable secrets, by tenant: %v; want one for org-a and none else", lines)
		}
		if strings.Contains(f.logs.String(), "plain-") {
			t.Error("the log shows a verifier secret")
		}
		// The answer is believed for a while: neither opened nor logged again.
		opens := f.box.opens.Load()
		for range 3 {
			if status, _ := f.readyz(); status != http.StatusOK {
				t.Fatalf("/readyz again: %d", status)
			}
		}
		if n := f.box.opens.Load(); n != opens || secretLines(f)["org-a"] != 1 {
			t.Errorf("later probes opened %d more secrets, and the log has %d lines for org-a", n-opens, secretLines(f)["org-a"])
		}
	})

	// Tenants whose inboxes come in turn: the one whose key is gone is logged.
	t.Run("InterleavedTenantsWithOneFailing", func(t *testing.T) {
		var tenants []string
		for range 20 {
			tenants = append(tenants, "org-a", "org-b")
		}
		f := newFixture(t, h, secretInboxes(tenants, "org-b"))
		if status, body := f.readyz(); status != http.StatusOK {
			t.Fatalf("/readyz: %d %s; want 200", status, body)
		}
		if lines := secretLines(f); lines["org-b"] != 1 || len(lines) != 1 {
			t.Errorf("log lines by tenant: %v; want one for org-b", lines)
		}
	})

	// When nothing opens, whatever the tenant, the server is not ready: a wrong master
	// key, or a key service that fails for everyone, looks like this.
	t.Run("EveryTenantsSecretsFail", func(t *testing.T) {
		f := newFixture(t, h, secretInboxes(append(repeat("org-a", 30), repeat("org-b", 3)...), "org-a", "org-b"))
		status, body := f.readyz()
		if status != http.StatusServiceUnavailable {
			t.Fatalf("/readyz with every key gone: %d %s; want 503", status, body)
		}
		var e struct{ Error struct{ Code string } }
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error.Code != "not_ready" {
			t.Errorf("the answer: %s; want code not_ready", body)
		}
		for _, leak := range []string{"hook-", "org-a", "org-b", "plain-"} {
			if strings.Contains(body, leak) {
				t.Errorf("the answer names %q: %s", leak, body)
			}
		}
		if lines := secretLines(f); lines["org-a"] != 1 || lines["org-b"] != 1 {
			t.Errorf("log lines by tenant: %v; want one each for org-a and org-b", lines)
		}
		if strings.Contains(f.logs.String(), "plain-") {
			t.Error("the log shows a verifier secret")
		}
	})

	// With one tenant only, its key being gone cannot be told from a wrong master key.
	t.Run("TheOnlyTenantsSecretsFail", func(t *testing.T) {
		f := newFixture(t, h, secretInboxes(repeat("org-a", 10), "org-a"))
		if status, body := f.readyz(); status != http.StatusServiceUnavailable {
			t.Fatalf("/readyz: %d %s; want 503", status, body)
		}
		if lines := secretLines(f); lines["org-a"] != 1 {
			t.Errorf("log lines by tenant: %v; want one for org-a", lines)
		}
	})

	// Every secret opens: ready.
	t.Run("EverySecretOpens", func(t *testing.T) {
		f := newFixture(t, h, secretInboxes(append(repeat("org-a", 4), repeat("org-b", 4)...)))
		if status, body := f.readyz(); status != http.StatusOK {
			t.Errorf("/readyz: %d %s; want 200", status, body)
		}
		if lines := secretLines(f); len(lines) != 0 {
			t.Errorf("log lines about unreadable secrets: %v; want none", lines)
		}
	})
}
