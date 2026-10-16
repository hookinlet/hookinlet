// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// orgPrincipal is a caller that belongs to one tenant and reads every inbox in it.
type orgPrincipal struct {
	tenant string
	admin  bool
}

func (p orgPrincipal) CanReadInbox(in *inbox.Inbox) bool { return in != nil && in.Tenant == p.tenant }
func (p orgPrincipal) CanAdmin() bool                    { return p.admin }
func (p orgPrincipal) Tenant() string                    { return p.tenant }

// unscopedPrincipal reads every inbox and belongs to no tenant, like the shipped
// admin token.
type unscopedPrincipal struct{}

func (unscopedPrincipal) CanReadInbox(in *inbox.Inbox) bool { return in != nil }
func (unscopedPrincipal) CanAdmin() bool                    { return true }

// tenantAuth knows a reader and an admin of each of two organizations, and an
// operator that is in none.
type tenantAuth struct{}

func (tenantAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
	case "reader-a":
		return orgPrincipal{tenant: "org-a"}, nil
	case "reader-b":
		return orgPrincipal{tenant: "org-b"}, nil
	case "admin-a":
		return orgPrincipal{tenant: "org-a", admin: true}, nil
	case "admin-b":
		return orgPrincipal{tenant: "org-b", admin: true}, nil
	case "operator":
		return unscopedPrincipal{}, nil
	}
	return nil, inbox.ErrUnauthenticated
}

// tenantEnv has an inbox called "alerts" in each of two tenants, and "only-b" in the
// second, each holding one message that says whose it is.
func tenantEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t, func(o *Options) { o.Authenticator = tenantAuth{}; o.Notifier = local.New() })
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
	return e, serveOnWith(t, e, net.ListenConfig{})
}

// A slug means the caller's own inbox, so two organizations can both have "alerts".
func TestTheSameSlugReachesEachTenantsOwnInbox(t *testing.T) {
	e, _ := tenantEnv(t)
	for tok, c := range map[string]struct{ want, other string }{
		"reader-a": {"from-a", "from-b"}, "reader-b": {"from-b", "from-a"},
	} {
		rec := e.get("/v1/inboxes/alerts/messages?after=0", tok)
		if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, c.want) || strings.Contains(body, c.other) {
			t.Errorf("%s reads (%d) %s, want only %q", tok, rec.Code, body, c.want)
		}
	}
}

// An inbox in another tenant does not exist as far as the caller can tell, and a caller
// that is in no tenant names inboxes in the default one, where there are none here.
func TestAnotherTenantsInboxIsNotFound(t *testing.T) {
	e, _ := tenantEnv(t)
	for _, c := range []struct{ tok, path string }{
		{"reader-a", "/v1/inboxes/only-b"},
		{"reader-a", "/v1/inboxes/only-b/messages"},
		{"reader-a", "/v1/inboxes/only-b/messages/1"},
		{"operator", "/v1/inboxes/alerts"},
	} {
		if rec := e.get(c.path, c.tok); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c.tok, c.path, rec.Code)
		}
	}
}

// The admin API works in the admin's tenant: it creates there, lists only there, and
// cannot name an inbox elsewhere.
func TestTheAdminAPIStaysInItsTenant(t *testing.T) {
	e, _ := tenantEnv(t)
	call := func(method, path, body, tok string) *struct {
		code int
		body string
	} {
		h := http.Header{"Authorization": {"Bearer " + tok}, "Content-Type": {"application/json"}}
		rec := e.do(req{method: method, path: path, body: body, headers: h})
		return &struct {
			code int
			body string
		}{rec.Code, rec.Body.String()}
	}
	if r := call(http.MethodPost, "/v1/admin/inboxes", `{"slug":"fresh","verifier":"none"}`, "admin-a"); r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	if in, err := e.store.GetInboxBySlug(inbox.WithTenant(t.Context(), "org-a"), "fresh"); err != nil || in.Tenant != "org-a" {
		t.Errorf("the inbox an org-a admin created: %+v, %v", in, err)
	}
	// The same slug in the other tenant is not a conflict.
	if r := call(http.MethodPost, "/v1/admin/inboxes", `{"slug":"fresh","verifier":"none"}`, "admin-b"); r.code != http.StatusCreated {
		t.Errorf("the same slug in another tenant: %d %s", r.code, r.body)
	}
	if r := call(http.MethodPost, "/v1/admin/inboxes", `{"slug":"fresh","verifier":"none"}`, "admin-a"); r.code != http.StatusConflict {
		t.Errorf("the same slug twice in one tenant: %d, want 409", r.code)
	}
	list := call(http.MethodGet, "/v1/admin/inboxes", "", "admin-a")
	if strings.Contains(list.body, "only-b") || strings.Count(list.body, `"slug"`) != 2 {
		t.Errorf("an org-a admin lists %s, want alerts and fresh only", list.body)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/admin/inboxes/only-b", ""},
		{http.MethodPatch, "/v1/admin/inboxes/only-b", `{"name":"mine now"}`},
		{http.MethodDelete, "/v1/admin/inboxes/only-b", ""},
		{http.MethodPut, "/v1/admin/inboxes/only-b/secret", `{"secret":"x"}`},
	} {
		if r := call(c.method, c.path, c.body, "admin-a"); r.code != http.StatusNotFound {
			t.Errorf("org-a admin %s %s: %d, want 404", c.method, c.path, r.code)
		}
	}
	if in, err := e.store.GetInboxBySlug(inbox.WithTenant(t.Context(), "org-b"), "only-b"); err != nil || in.Name != "only-b" {
		t.Errorf("org-b's inbox after org-a's attempts: %+v, %v", in, err)
	}
}

// MCP calls run detached from the request, so the tools scope their own context.
func TestMCPToolsStayInTheCallersTenant(t *testing.T) {
	_, base := tenantEnv(t)
	cs := connectMCP(t, base, "reader-a")
	p := callTool(t, cs, "query_messages", map[string]any{"inbox": "alerts", "after": 0, "bodies": true}).page(t)
	if len(p.Messages) != 1 || !strings.Contains(mustJSON(p.Messages[0]), "from-a") {
		t.Errorf("org-a's alerts: %v", p.Messages)
	}
	if o := callTool(t, cs, "query_messages", map[string]any{"inbox": "only-b", "after": 0}); o.err == "" {
		t.Errorf("org-a read an inbox of org-b: %s", o.raw)
	}
	var l struct {
		Inboxes []struct {
			Slug string `json:"slug"`
		} `json:"inboxes"`
	}
	if err := json.Unmarshal(callTool(t, cs, "list_inboxes", nil).raw, &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Inboxes) != 1 || l.Inboxes[0].Slug != "alerts" {
		t.Errorf("org-a lists %+v, want only alerts", l.Inboxes)
	}
}
