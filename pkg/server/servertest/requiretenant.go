// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package servertest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/server"
)

func runRequireTenant(t *testing.T, h Harness) {
	t.Run("RefusesAPrincipalWithoutATenant", func(t *testing.T) {
		f := newFixture(t, h, config{mods: func(o *server.Options) { o.RequireTenant = true }})
		if f.callers.NoTenant == nil {
			t.Skip("Callers.NoTenant is not set")
		}
		for _, path := range []string{
			"/v1/inboxes/alerts", "/v1/inboxes/alerts/messages?after=0", "/v1/inboxes/alerts/messages/1",
			"/v1/inboxes/no-such-inbox", "/v1/admin/inboxes", "/v1/admin/status",
		} {
			if status, body := f.get(path, f.callers.NoTenant); status != http.StatusInternalServerError || !isInternal(body) {
				t.Errorf("GET %s: %d %s; want 500 internal, which says nothing of why", path, status, body)
			}
		}
		admin := with(f.callers.NoTenant, "Content-Type", "application/json")
		if status, body := f.do(http.MethodPost, "/v1/admin/inboxes", admin, `{"slug":"mine","verifier":"none"}`); status != http.StatusInternalServerError {
			t.Errorf("admin create: %d %s; want 500", status, body)
		}
		if status, got := f.stream("/v1/inboxes/alerts/stream?after=0", f.callers.NoTenant, "", 2*time.Second); status != http.StatusInternalServerError {
			t.Errorf("stream: %d %q; want 500", status, got)
		}
		for name, body := range map[string]string{
			"initialize":     `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
			"tools/list":     `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			"list_inboxes":   `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_inboxes","arguments":{}}}`,
			"query_messages": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_messages","arguments":{"inbox":"alerts","after":0}}}`,
		} {
			hdr := with(f.callers.NoTenant, "Content-Type", "application/json", "Accept", "application/json, text/event-stream")
			status, resp := f.do(http.MethodPost, "/mcp", hdr, body)
			if status != http.StatusInternalServerError || !isInternal(resp) {
				t.Errorf("MCP %s: %d %s; want 500 internal", name, status, resp)
			}
			for _, leak := range []string{"org-a", "org-b", "only-b", "dflt-inbox", "from-"} {
				if strings.Contains(resp, leak) {
					t.Errorf("MCP %s answers with %q: %s", name, leak, resp)
				}
			}
		}
		if logs := f.logs.String(); !strings.Contains(logs, "RequireTenant") || !strings.Contains(logs, `"level":"ERROR"`) {
			t.Errorf("the log does not name the cause at error level:\n%s", logs)
		}
	})

	// The option changes nothing for a caller that is in a tenant, or an admin that is in none.
	t.Run("ServesTenantsAndTheOperator", func(t *testing.T) {
		f := newFixture(t, h, config{mods: func(o *server.Options) { o.RequireTenant = true }})
		status, body := f.get("/v1/inboxes/alerts/messages?after=0", f.callers.ReaderA)
		if status != http.StatusOK || !strings.Contains(body, bodyA) || strings.Contains(body, bodyB) {
			t.Errorf("ReaderA: %d %s", status, body)
		}
		if status, got := f.stream("/v1/inboxes/alerts/stream?after=0", f.callers.ReaderA, bodyA, 5*time.Second); status != http.StatusOK {
			t.Errorf("ReaderA's stream: %d %q", status, got)
		}
		if out, isErr := f.tool(f.mcpSession(f.callers.ReaderA), "list_inboxes", nil); isErr || !sameSet(slugsIn(t, out), "alerts") {
			t.Errorf("ReaderA list_inboxes: %v %s; want its alerts only", isErr, out)
		}
		if f.callers.AdminA != nil {
			if status, body := f.get("/v1/admin/inboxes", f.callers.AdminA); status != http.StatusOK || strings.Contains(body, "only-b") {
				t.Errorf("an org-a admin lists %d %s", status, body)
			}
		}
		if status, body := f.get("/v1/admin/inboxes", f.callers.ReaderA); status != http.StatusForbidden {
			t.Errorf("a tenant's reader on the admin API: %d %s; want 403", status, body)
		}
		if status, body := f.get("/v1/admin/status", f.callers.Operator); status != http.StatusOK {
			t.Errorf("the operator's status: %d %s", status, body)
		}
		if status, body := f.get("/v1/admin/inboxes", f.callers.Operator); status != http.StatusOK {
			t.Errorf("the operator lists: %d %s", status, body)
		}
		if status, body := f.get("/v1/inboxes/dflt-inbox", f.callers.Operator); status != http.StatusOK {
			t.Errorf("the operator reads a default-tenant inbox: %d %s", status, body)
		}
		if strings.Contains(f.logs.String(), "RequireTenant") {
			t.Errorf("a refusal was logged for callers that were served:\n%s", f.logs.String())
		}
	})

	// Without the option nothing changes, including the trap it is there for.
	t.Run("WithoutItAPrincipalWithoutATenantIsUnscoped", func(t *testing.T) {
		f := newFixture(t, h, config{})
		if f.callers.NoTenant == nil {
			t.Skip("Callers.NoTenant is not set")
		}
		if status, body := f.get("/v1/inboxes/alerts", f.callers.NoTenant); status != http.StatusNotFound {
			t.Errorf("a slug only other tenants have: %d %s; want 404", status, body)
		}
		if status, body := f.get("/v1/inboxes/dflt-inbox", f.callers.NoTenant); status != http.StatusOK {
			t.Errorf("the default tenant's inbox: %d %s; want 200", status, body)
		}
		if status, got := f.stream("/v1/inboxes/dflt-inbox/stream?after=0", f.callers.NoTenant, bodyDef, 5*time.Second); status != http.StatusOK {
			t.Errorf("stream: %d %q; want 200", status, got)
		}
		if out, isErr := f.tool(f.mcpSession(f.callers.NoTenant), "list_inboxes", nil); isErr || len(slugsIn(t, out)) != 4 {
			t.Errorf("list_inboxes: %v %s; want every tenant's inboxes", isErr, out)
		}
		if status, _ := f.get("/v1/admin/inboxes", f.callers.NoTenant); status != http.StatusForbidden {
			t.Errorf("the admin API: %d, want 403", status)
		}
		if strings.Contains(f.logs.String(), "RequireTenant") {
			t.Errorf("a refusal was logged without the option:\n%s", f.logs.String())
		}
	})
}

// isInternal reports whether body is the standard answer to an internal error, which
// says nothing about why.
func isInternal(body string) bool {
	var e struct {
		Error struct{ Code, Message string }
	}
	return json.Unmarshal([]byte(body), &e) == nil && e.Error.Code == "internal" && e.Error.Message == "internal error"
}
