// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package servertest

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func runTenants(t *testing.T, h Harness) {
	cases := []struct {
		name string
		fn   func(*testing.T, *fixture)
	}{
		{"TheSameSlugReachesEachTenantsOwnInbox", testSameSlug},
		{"AnotherTenantsInboxIsNotFound", testOtherTenantIs404},
		{"AnUnscopedPrincipalNamesTheDefaultTenant", testUnscopedIsDefaultTenant},
		{"StreamsStayInTheCallersTenant", testStreamScope},
		{"MCPToolsStayInTheCallersTenant", testMCPScope},
		{"TheAdminAPIStaysInTheAdminsTenant", testAdminScope},
		{"DeliveriesGoToTheInboxOfTheirKey", testIngestScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newFixture(t, h, config{})) })
	}
}

// A slug names the caller's own inbox, so two tenants can both have "alerts".
func testSameSlug(t *testing.T, f *fixture) {
	for name, c := range map[string]struct {
		cred        http.Header
		want, other string
	}{
		"ReaderA": {f.callers.ReaderA, bodyA, bodyB},
		"ReaderB": {f.callers.ReaderB, bodyB, bodyA},
	} {
		for _, path := range []string{"/v1/inboxes/alerts/messages?after=0", "/v1/inboxes/alerts/messages/1"} {
			status, body := f.get(path, c.cred)
			if status != http.StatusOK || !strings.Contains(body, c.want) || strings.Contains(body, c.other) {
				t.Errorf("%s GET %s: %d %s; want only %q", name, path, status, body, c.want)
			}
		}
	}
}

// An inbox of another tenant does not exist as far as the caller can tell: 404, not 403.
func testOtherTenantIs404(t *testing.T, f *fixture) {
	for _, c := range []struct {
		who, path string
		cred      http.Header
	}{
		{"ReaderA", "/v1/inboxes/only-b", f.callers.ReaderA},
		{"ReaderA", "/v1/inboxes/only-b/messages", f.callers.ReaderA},
		{"ReaderA", "/v1/inboxes/only-b/messages/1", f.callers.ReaderA},
		{"ReaderA", "/v1/inboxes/only-b/messages/1/raw", f.callers.ReaderA},
		{"ReaderA", "/v1/inboxes/dflt-inbox", f.callers.ReaderA},
		{"ReaderB", "/v1/inboxes/dflt-inbox", f.callers.ReaderB},
	} {
		if status, body := f.get(c.path, c.cred); status != http.StatusNotFound || strings.Contains(body, bodyOnB) {
			t.Errorf("%s GET %s: %d %s; want 404", c.who, c.path, status, body)
		}
	}
	// What it names is not found, whatever the slug is in another tenant.
	if status, _ := f.get("/v1/inboxes/scope-key-b", f.callers.ReaderB); status != http.StatusNotFound {
		t.Errorf("an ingest key used as a slug: %d, want 404", status)
	}
}

// A principal that belongs to no tenant names inboxes in the default tenant: it reaches
// the default tenant's inbox and not an inbox another tenant has by that slug.
func testUnscopedIsDefaultTenant(t *testing.T, f *fixture) {
	status, body := f.get("/v1/inboxes/dflt-inbox/messages?after=0", f.callers.Operator)
	if status != http.StatusOK || !strings.Contains(body, bodyDef) {
		t.Errorf("the operator reads the default tenant's inbox: %d %s", status, body)
	}
	for _, path := range []string{"/v1/inboxes/alerts", "/v1/inboxes/alerts/messages?after=0", "/v1/inboxes/only-b"} {
		if status, body := f.get(path, f.callers.Operator); status != http.StatusNotFound {
			t.Errorf("the operator GET %s: %d %s; want 404, as these inboxes are not in the default tenant", path, status, body)
		}
	}
}

// A stream is scoped when it opens (and checked again while it runs).
func testStreamScope(t *testing.T, f *fixture) {
	for name, c := range map[string]struct {
		cred        http.Header
		want, other string
	}{
		"ReaderA": {f.callers.ReaderA, bodyA, bodyB},
		"ReaderB": {f.callers.ReaderB, bodyB, bodyA},
	} {
		status, got := f.stream("/v1/inboxes/alerts/stream?after=0", c.cred, c.want, 5*time.Second)
		if status != http.StatusOK || !strings.Contains(got, c.want) || strings.Contains(got, c.other) {
			t.Errorf("%s follows alerts: %d %q; want %q and not %q", name, status, got, c.want, c.other)
		}
	}
	for _, c := range []struct {
		who, path string
		cred      http.Header
	}{
		{"ReaderA", "/v1/inboxes/only-b/stream?after=0", f.callers.ReaderA},
		{"ReaderA", "/v1/inboxes/dflt-inbox/stream?after=0", f.callers.ReaderA},
		{"Operator", "/v1/inboxes/alerts/stream?after=0", f.callers.Operator},
	} {
		if status, got := f.stream(c.path, c.cred, "", time.Second); status != http.StatusNotFound {
			t.Errorf("%s follows %s: %d %q; want 404", c.who, c.path, status, got)
		}
	}
}

// MCP calls run detached from the request, so the tools scope their own context.
func testMCPScope(t *testing.T, f *fixture) {
	a := f.mcpSession(f.callers.ReaderA)
	out, isErr := f.tool(a, "query_messages", map[string]any{"inbox": "alerts", "after": 0, "bodies": true})
	if isErr || !strings.Contains(out, bodyA) || strings.Contains(out, bodyB) {
		t.Errorf("ReaderA query_messages alerts: %v %s; want only %q", isErr, out, bodyA)
	}
	for _, name := range []string{"query_messages", "get_message"} {
		args := map[string]any{"inbox": "only-b", "after": 0, "seq": 1}
		if out, isErr := f.tool(a, name, args); !isErr || strings.Contains(out, bodyOnB) {
			t.Errorf("ReaderA %s on another tenant's inbox: error %v, %s; want a tool error that shows nothing", name, isErr, out)
		}
	}
	if out, isErr := f.tool(a, "list_inboxes", nil); isErr || !sameSet(slugsIn(t, out), "alerts") {
		t.Errorf("ReaderA list_inboxes: %v %s; want only its alerts", isErr, out)
	}
	b := f.mcpSession(f.callers.ReaderB)
	if out, isErr := f.tool(b, "list_inboxes", nil); isErr || !sameSet(slugsIn(t, out), "alerts", "only-b") {
		t.Errorf("ReaderB list_inboxes: %v %s; want alerts and only-b", isErr, out)
	}

	// The operator names the default tenant, and lists what it can read in every tenant.
	op := f.mcpSession(f.callers.Operator)
	if out, isErr := f.tool(op, "query_messages", map[string]any{"inbox": "alerts", "after": 0}); !isErr {
		t.Errorf("the operator read a slug that is in no default-tenant inbox: %s", out)
	}
	if out, isErr := f.tool(op, "query_messages", map[string]any{"inbox": "dflt-inbox", "after": 0, "bodies": true}); isErr || !strings.Contains(out, bodyDef) {
		t.Errorf("the operator reads the default tenant's inbox: %v %s", isErr, out)
	}
	if out, isErr := f.tool(op, "list_inboxes", nil); isErr || !sameSet(slugsIn(t, out), "alerts", "alerts", "only-b", "dflt-inbox") {
		t.Errorf("the operator list_inboxes: %v %s; want every tenant's inboxes", isErr, out)
	}
}

// The admin API works in the admin's tenant: it creates there, lists only there, and
// cannot name an inbox elsewhere. A tenant's reader cannot use it.
func testAdminScope(t *testing.T, f *fixture) {
	if f.callers.AdminA == nil {
		t.Skip("Callers.AdminA is not set")
	}
	admin := with(f.callers.AdminA, "Content-Type", "application/json")
	if status, body := f.do(http.MethodPost, "/v1/admin/inboxes", admin, `{"slug":"fresh","verifier":"none"}`); status != http.StatusCreated {
		t.Fatalf("an org-a admin creates an inbox: %d %s", status, body)
	}
	if in, err := f.store.GetInboxBySlug(inbox.WithTenant(t.Context(), "org-a"), "fresh"); err != nil || in.Tenant != "org-a" {
		t.Errorf("the inbox an org-a admin created: %+v, %v; want it in org-a", in, err)
	}
	if status, _ := f.do(http.MethodPost, "/v1/admin/inboxes", admin, `{"slug":"fresh","verifier":"none"}`); status != http.StatusConflict {
		t.Errorf("the same slug twice in one tenant: %d, want 409", status)
	}
	// "alerts" exists in org-b too, so a create of it must conflict with org-a's own.
	if status, _ := f.do(http.MethodPost, "/v1/admin/inboxes", admin, `{"slug":"alerts","verifier":"none"}`); status != http.StatusConflict {
		t.Errorf("creating the slug org-a already has: %d, want 409", status)
	}
	status, body := f.get("/v1/admin/inboxes", admin)
	if status != http.StatusOK || !sameSet(slugsIn(t, body), "alerts", "fresh") {
		t.Errorf("an org-a admin lists %d %s; want alerts and fresh only", status, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/admin/inboxes/only-b", ""},
		{http.MethodPatch, "/v1/admin/inboxes/only-b", `{"name":"mine now"}`},
		{http.MethodDelete, "/v1/admin/inboxes/only-b", ""},
		{http.MethodPut, "/v1/admin/inboxes/only-b/secret", `{"secret":"x"}`},
		{http.MethodGet, "/v1/admin/inboxes/dflt-inbox", ""},
	} {
		if status, body := f.do(c.method, c.path, admin, c.body); status != http.StatusNotFound {
			t.Errorf("org-a admin %s %s: %d %s; want 404", c.method, c.path, status, body)
		}
	}
	if in, err := f.store.GetInboxBySlug(inbox.WithTenant(t.Context(), "org-b"), "only-b"); err != nil || in.Name != "only-b" {
		t.Errorf("org-b's inbox after org-a's attempts: %+v, %v", in, err)
	}
	if status, body := f.get("/v1/admin/inboxes", f.callers.ReaderA); status != http.StatusForbidden {
		t.Errorf("a tenant's reader on the admin API: %d %s; want 403", status, body)
	}
}

// A delivery goes to the inbox its ingest key names, in that inbox's tenant, whoever
// sends it, and only that tenant reads it.
func testIngestScope(t *testing.T, f *fixture) {
	if status, body := f.do(http.MethodPost, "/in/scope-key-a", nil, "second-for-a"); status/100 != 2 {
		t.Fatalf("a second delivery to org-a's key: %d %s", status, body)
	}
	status, body := f.get("/v1/inboxes/alerts/messages?after=0", f.callers.ReaderA)
	if status != http.StatusOK || !strings.Contains(body, "second-for-a") || strings.Contains(body, bodyB) {
		t.Errorf("ReaderA after a delivery to its inbox: %d %s", status, body)
	}
	if _, body := f.get("/v1/inboxes/alerts/messages?after=0", f.callers.ReaderB); strings.Contains(body, "second-for-a") {
		t.Errorf("ReaderB sees a delivery made to org-a's key: %s", body)
	}
	if status, _ := f.do(http.MethodPost, "/in/no-such-key", nil, "x"); status != http.StatusNotFound {
		t.Errorf("a delivery to an unknown key: %d, want 404", status)
	}
}
