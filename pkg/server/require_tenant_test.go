// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// carelessPrincipal is the mistake Options.RequireTenant is there to catch: a customer's
// principal that says yes to every inbox and forgot Tenant().
type carelessPrincipal struct{}

func (carelessPrincipal) CanReadInbox(in *inbox.Inbox) bool { return in != nil }
func (carelessPrincipal) CanAdmin() bool                    { return false }

// carelessToken is long and distinctive so that a test can tell it is not in a log line.
const carelessToken = "careless-7f3a9c1e5b2d" //nolint:gosec // a fake value; the test proves it is never logged

// requireTenantAuth is tenantAuth, plus the careless principal, plus one ("flips") that
// is org-a's reader until flipped and careless after.
type requireTenantAuth struct{ flipped *atomic.Bool }

func (a requireTenantAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
	case carelessToken:
		return carelessPrincipal{}, nil
	case "flips":
		if a.flipped.Load() {
			return carelessPrincipal{}, nil
		}
		return orgPrincipal{tenant: "org-a"}, nil
	}
	return tenantAuth{}.Authenticate(r)
}

// requireTenantEnv is tenantEnv with RequireTenant set as asked, and the switch that
// flips the "flips" credential.
func requireTenantEnv(t *testing.T, require bool) (*env, string, *atomic.Bool) {
	t.Helper()
	flipped := &atomic.Bool{}
	e := newEnv(t, func(o *Options) {
		o.Authenticator = requireTenantAuth{flipped: flipped}
		o.RequireTenant = require
		o.Notifier = local.New()
		o.StreamRevalidateInterval = 100 * time.Millisecond
	})
	for _, c := range []struct{ tenant, slug, key, body string }{
		{"org-a", "alerts", "key-a", "from-a"},
		{"org-b", "alerts", "key-b", "from-b"},
		{"org-b", "only-b", "key-only-b", "only-b"},
	} {
		in := &inbox.Inbox{Slug: c.slug, IngestKey: c.key, Name: c.slug, Verifier: "trusted"}
		if err := e.store.CreateInbox(inbox.WithTenant(t.Context(), c.tenant), in); err != nil {
			t.Fatal(err)
		}
		if rec := e.post("/in/"+c.key, c.body); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("delivery to %s: %d %s", c.key, rec.Code, rec.Body)
		}
	}
	return e, serveOnWith(t, e, net.ListenConfig{}), flipped
}

// isInternal reports whether body is the standard answer to an internal error, which
// says nothing about why.
func isInternal(body string) bool {
	var e struct {
		Error struct{ Code, Message string }
	}
	return json.Unmarshal([]byte(body), &e) == nil && e.Error.Code == "internal" && e.Error.Message == "internal error"
}

// With RequireTenant, a principal that is in no tenant and is not an admin is refused
// everywhere it could authenticate: the read API, the stream, MCP (before the session
// starts, and on every tool call), and the admin API. The caller learns nothing of why;
// the log says.
func TestRequireTenantRefusesAPrincipalWithoutATenant(t *testing.T) {
	e, base, _ := requireTenantEnv(t, true)
	for _, path := range []string{
		"/v1/inboxes/alerts", "/v1/inboxes/alerts/messages?after=0", "/v1/inboxes/alerts/messages/1",
		"/v1/inboxes/alerts/messages/1/raw", "/v1/inboxes/no-such-inbox",
		"/v1/admin/inboxes", "/v1/admin/status", "/v1/admin/tokens",
	} {
		if rec := e.get(path, carelessToken); rec.Code != http.StatusInternalServerError || !isInternal(rec.Body.String()) {
			t.Errorf("GET %s: %d %s, want 500 internal", path, rec.Code, rec.Body)
		}
	}
	// /metrics lists no 500 of its own (its "default" response covers it), so it is asked
	// over the socket, past the check that every status is one the document names.
	if status, body := do2(t, http.MethodGet, base+"/metrics", http.Header{"Authorization": {"Bearer " + carelessToken}}, ""); status != http.StatusInternalServerError || !isInternal(body) {
		t.Errorf("GET /metrics: %d %s, want 500 internal", status, body)
	}
	h := http.Header{"Authorization": {"Bearer " + carelessToken}, "Content-Type": {"application/json"}}
	if rec := e.do(req{method: http.MethodPost, path: "/v1/admin/inboxes", body: `{"slug":"mine","verifier":"none"}`, headers: h}); rec.Code != http.StatusInternalServerError {
		t.Errorf("admin create: %d %s, want 500", rec.Code, rec.Body)
	}
	if s := openStream(t, base+"/v1/inboxes/alerts/stream", carelessToken, nil); s.status != http.StatusInternalServerError {
		t.Errorf("stream: status %d, want 500", s.status)
	}
	for name, body := range map[string]string{
		"initialize":        `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		"tools/list":        `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		"list_inboxes":      `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_inboxes","arguments":{}}}`,
		"query_messages":    `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_messages","arguments":{"inbox":"alerts","after":0}}}`,
		"get_message":       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_message","arguments":{"inbox":"alerts","seq":1}}}`,
		"wait_for_messages": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait_for_messages","arguments":{"inbox":"alerts","after":0,"timeout_seconds":0}}}`,
	} {
		status, _, resp := mcpPost(t, base, carelessToken, body)
		if status != http.StatusInternalServerError || !isInternal(resp) {
			t.Errorf("MCP %s: %d %s, want 500 internal", name, status, resp)
		}
		for _, leak := range []string{"org-a", "org-b", "only-b", "from-"} {
			if strings.Contains(resp, leak) {
				t.Errorf("MCP %s answers with %q: %s", name, leak, resp)
			}
		}
	}

	logs := e.logs.String()
	if !strings.Contains(logs, "carelessPrincipal") || !strings.Contains(logs, "RequireTenant") || !strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("the log does not name the cause at error level:\n%s", logs)
	}
	if strings.Contains(logs, carelessToken) {
		t.Error("the log contains the credential")
	}
}

// A principal in a tenant is served as before, and so is an admin in no tenant (the
// operator), which is the one view across every tenant a hosted operator has.
func TestRequireTenantServesTenantsAndTheOperator(t *testing.T) {
	e, base, _ := requireTenantEnv(t, true)

	rec := e.get("/v1/inboxes/alerts/messages?after=0", "reader-a")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from-a") || strings.Contains(rec.Body.String(), "from-b") {
		t.Errorf("reader-a: %d %s", rec.Code, rec.Body)
	}
	s := openStream(t, base+"/v1/inboxes/alerts/stream", "reader-a", nil)
	if s.status != http.StatusOK {
		t.Errorf("reader-a's stream: status %d", s.status)
	}
	cs := connectMCP(t, base, "reader-a")
	var l struct {
		Inboxes []struct {
			Slug string `json:"slug"`
		} `json:"inboxes"`
	}
	if err := json.Unmarshal(callTool(t, cs, "list_inboxes", nil).raw, &l); err != nil || len(l.Inboxes) != 1 || l.Inboxes[0].Slug != "alerts" {
		t.Errorf("reader-a lists %+v (%v), want its alerts only", l.Inboxes, err)
	}

	// An admin in a tenant keeps to its tenant, as it does without the option.
	if rec := e.get("/v1/admin/inboxes", "admin-a"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "only-b") {
		t.Errorf("admin-a lists %d %s", rec.Code, rec.Body)
	}
	// A tenant's reader still cannot use the admin API.
	if rec := e.get("/v1/admin/inboxes", "reader-a"); rec.Code != http.StatusForbidden {
		t.Errorf("reader-a on the admin API: %d, want 403", rec.Code)
	}

	// The operator is in no tenant and is an admin.
	if rec := e.get("/v1/admin/status", "operator"); rec.Code != http.StatusOK {
		t.Errorf("operator status: %d %s", rec.Code, rec.Body)
	}
	if rec := e.get("/v1/admin/inboxes", "operator"); rec.Code != http.StatusOK {
		t.Errorf("operator lists: %d %s", rec.Code, rec.Body)
	}
	if rec := e.get("/v1/inboxes/alerts", "operator"); rec.Code != http.StatusNotFound {
		t.Errorf("operator reads a default-tenant slug that does not exist: %d, want 404", rec.Code)
	}
	if err := json.Unmarshal(callTool(t, connectMCP(t, base, "operator"), "list_inboxes", nil).raw, &l); err != nil || len(l.Inboxes) != 3 {
		t.Errorf("operator lists %+v (%v), want every inbox", l.Inboxes, err)
	}
	if strings.Contains(e.logs.String(), "RequireTenant") {
		t.Errorf("a refusal was logged for a caller that was served:\n%s", e.logs)
	}
}

// A stream and an MCP wait end when their credential is no longer in a tenant, as they
// do when it is revoked.
func TestRequireTenantEndsAStreamWhoseCredentialLosesItsTenant(t *testing.T) {
	_, base, flipped := requireTenantEnv(t, true)
	s := openStream(t, base+"/v1/inboxes/alerts/stream", "flips", nil)
	if s.status != http.StatusOK {
		t.Fatalf("stream: status %d", s.status)
	}
	cs := connectMCP(t, base, "flips")
	done := make(chan toolOut, 1)
	go func() {
		done <- callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "alerts", "after": 1, "timeout_seconds": 60})
	}()
	time.Sleep(300 * time.Millisecond)
	flipped.Store(true)
	if !s.ended(3 * time.Second) {
		t.Error("the stream is still open")
	}
	select {
	case o := <-done:
		if o.err == "" {
			t.Errorf("the wait ended with %s, want an error", o.raw)
		}
	case <-time.After(5 * time.Second):
		t.Error("the wait is still running")
	}
}

// Without RequireTenant nothing changes, including the trap it is there for: a careless
// principal in no tenant lists every tenant's inboxes, and reads a default-tenant inbox
// that has the slug it names.
func TestWithoutRequireTenantAPrincipalWithoutATenantIsUnscoped(t *testing.T) {
	e, base, _ := requireTenantEnv(t, false)
	var l struct {
		Inboxes []struct {
			Slug string `json:"slug"`
		} `json:"inboxes"`
	}
	if err := json.Unmarshal(callTool(t, connectMCP(t, base, carelessToken), "list_inboxes", nil).raw, &l); err != nil || len(l.Inboxes) != 3 {
		t.Errorf("the careless principal lists %+v (%v), want every tenant's inboxes", l.Inboxes, err)
	}
	if rec := e.get("/v1/inboxes/alerts", carelessToken); rec.Code != http.StatusNotFound {
		t.Errorf("before a default-tenant alerts exists: %d, want 404", rec.Code)
	}
	e.newInbox("alerts", "trusted", "")
	if rec := e.get("/v1/inboxes/alerts", carelessToken); rec.Code != http.StatusOK {
		t.Errorf("the default tenant's alerts: %d %s, want 200", rec.Code, rec.Body)
	}
	if s := openStream(t, base+"/v1/inboxes/alerts/stream", carelessToken, nil); s.status != http.StatusOK {
		t.Errorf("stream: status %d, want 200", s.status)
	}
	if rec := e.get("/v1/admin/inboxes", carelessToken); rec.Code != http.StatusForbidden {
		t.Errorf("admin API: %d, want 403", rec.Code)
	}
	if strings.Contains(e.logs.String(), "RequireTenant") {
		t.Errorf("a refusal was logged without the option:\n%s", e.logs)
	}
}
