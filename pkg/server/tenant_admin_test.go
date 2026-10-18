// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// A tenant's administrator (a TenantPrincipal whose CanAdmin is true) works on its own
// tenant's inboxes and on nothing else. These tests try every admin operation as every
// kind of caller, against two tenants.

// plainReader reads every inbox, belongs to no tenant, and is not an admin.
type plainReader struct{}

func (plainReader) CanReadInbox(in *inbox.Inbox) bool { return in != nil }
func (plainReader) CanAdmin() bool                    { return false }

// matrixAuth is tenantAuth, plus a caller in no tenant that is not an admin ("plain"),
// plus the tokens the admin API issues (through the shipped token authenticator), so
// that a token a tenant admin manages to issue can be tried.
type matrixAuth struct{ tokens *token.Authenticator }

func (a matrixAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "plain" {
		return plainReader{}, nil
	}
	if p, err := (tenantAuth{}).Authenticate(r); err == nil {
		return p, nil
	}
	return a.tokens.Authenticate(r)
}

// matrixEnv holds, in tenant org-a, "alerts" and "only-a"; in org-b, "alerts" and
// "only-b"; and in the default tenant, "home" (the operator's, since it names inboxes
// there). Each has one message. A token that reads org-b's only-b is stored too.
type matrixEnv struct {
	*env
	base      string
	tokB      string // the token itself
	tokBID    inbox.TokenID
	tokBName  string
	tokenHash []byte
}

func newMatrixEnv(t *testing.T, mods ...func(*Options)) *matrixEnv {
	t.Helper()
	all := append([]func(*Options){func(o *Options) {
		o.Authenticator = matrixAuth{tokens: token.NewAuthenticator(o.Store, "")}
		o.Notifier = local.New()
		o.RetentionInterval = -1 // no sweep at start, which a run from the API would find busy
	}}, mods...)
	e := newEnv(t, all...)
	for _, c := range []struct{ tenant, slug, key string }{
		{"org-a", "alerts", "key-a"},
		{"org-a", "only-a", "key-only-a"},
		{"org-b", "alerts", "key-b"},
		{"org-b", "only-b", "key-only-b"},
		{"", "home", "key-home"},
	} {
		in := &inbox.Inbox{Slug: c.slug, IngestKey: c.key, Name: c.slug, Verifier: "trusted"}
		if err := e.store.CreateInbox(inbox.WithTenant(t.Context(), c.tenant), in); err != nil {
			t.Fatal(err)
		}
		if rec := e.post("/in/"+c.key, "message for "+c.tenant+"/"+c.slug); rec.Code != http.StatusOK {
			t.Fatalf("delivery to %s: %d %s", c.key, rec.Code, rec.Body)
		}
	}
	m := &matrixEnv{env: e, tokBName: "org-b-secret-agent"}
	onlyB, err := e.store.GetInboxBySlug(inbox.WithTenant(t.Context(), "org-b"), "only-b")
	if err != nil {
		t.Fatal(err)
	}
	tok, hash, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	rec := &inbox.Token{Name: m.tokBName, Hash: hash, InboxIDs: []inbox.ID{onlyB.ID}}
	if err := e.store.CreateToken(inbox.WithTenant(t.Context(), "org-b"), rec); err != nil {
		t.Fatal(err)
	}
	m.tokB, m.tokBID, m.tokenHash = tok, rec.ID, hash
	m.base = serveOn(t, e)
	return m
}

func (m *matrixEnv) call(method, path, body, tok string) *struct {
	code int
	body string
} {
	m.t.Helper()
	h := http.Header{"Authorization": {"Bearer " + tok}}
	if body != "" {
		h.Set("Content-Type", "application/json")
	}
	rec := m.do(req{method: method, path: path, body: body, headers: h})
	return &struct {
		code int
		body string
	}{rec.Code, rec.Body.String()}
}

// inboxState is what a refused or misdirected call must not change about an inbox.
func (m *matrixEnv) inboxState(tenant, slug string) string {
	m.t.Helper()
	in, err := m.store.GetInboxBySlug(inbox.WithTenant(m.t.Context(), tenant), slug)
	if err != nil {
		return "error: " + err.Error()
	}
	st, err := m.store.Stats(inbox.WithTenant(m.t.Context(), tenant), in.ID)
	if err != nil {
		return "error: " + err.Error()
	}
	return strings.Join([]string{in.Name, in.IngestKey, in.Verifier, strconv.Itoa(int(in.Version)),
		strconv.FormatBool(len(in.VerifierSecret) > 0), strconv.FormatBool(in.Disabled), strconv.FormatInt(st.MessageCount, 10)}, "|")
}

// adminCase is one admin operation: how to call it on the caller's own data, and, for
// one that names an inbox, on another tenant's.
type adminCase struct {
	method, path, body string // {own} is the caller's inbox, {tok} the id of org-b's token
	ok                 int    // the status of a call that works
	storeWide          bool   // the operation is the operator's only
	// cross is the same call aimed at the other tenant's inbox ({other}); it must be 404.
	cross bool
}

var adminMatrix = map[string]adminCase{
	"createInbox":       {method: "POST", path: "/v1/admin/inboxes", body: `{"slug":"fresh","verifier":"none"}`, ok: 201},
	"listInboxes":       {method: "GET", path: "/v1/admin/inboxes", ok: 200},
	"showInbox":         {method: "GET", path: "/v1/admin/inboxes/{own}", ok: 200, cross: true},
	"updateInbox":       {method: "PATCH", path: "/v1/admin/inboxes/{own}", body: `{"name":"renamed"}`, ok: 200, cross: true},
	"deleteInbox":       {method: "DELETE", path: "/v1/admin/inboxes/{own}", ok: 204, cross: true},
	"setVerifierSecret": {method: "PUT", path: "/v1/admin/inboxes/{own}/secret", body: `{"secret":"s3cret"}`, ok: 200, cross: true},
	"rotateIngestKey":   {method: "POST", path: "/v1/admin/inboxes/{own}/rotate-ingest-key", ok: 200, cross: true},
	"runRetention":      {method: "POST", path: "/v1/admin/retention/run", ok: 200, storeWide: true},
	"adminStatus":       {method: "GET", path: "/v1/admin/status", ok: 200, storeWide: true},
	"backup":            {method: "GET", path: "/v1/admin/backup", ok: 200, storeWide: true},
	"createToken":       {method: "POST", path: "/v1/admin/tokens", body: `{"name":"new","inboxes":["{own}"]}`, ok: 201, storeWide: true},
	"listTokens":        {method: "GET", path: "/v1/admin/tokens", ok: 200, storeWide: true},
	"revokeToken":       {method: "DELETE", path: "/v1/admin/tokens/{tok}", ok: 204, storeWide: true},
	"metrics":           {method: "GET", path: "/metrics", ok: 200, storeWide: true},
}

type matrixCaller struct {
	tok, tenant, own, other string
	admin, tenantScoped     bool
}

var matrixCallers = []matrixCaller{
	{tok: "operator", tenant: "", own: "home", admin: true},
	{tok: "admin-a", tenant: "org-a", own: "only-a", other: "only-b", admin: true, tenantScoped: true},
	{tok: "admin-b", tenant: "org-b", own: "only-b", other: "only-a", admin: true, tenantScoped: true},
	{tok: "reader-a", tenant: "org-a", own: "only-a", other: "only-b", tenantScoped: true},
	{tok: "plain", tenant: "", own: "home"},
}

func errorOf(body string) (code, msg string) {
	var e struct {
		Error struct{ Code, Message string }
	}
	_ = json.Unmarshal([]byte(body), &e)
	return e.Error.Code, e.Error.Message
}

// adminLines counts the admin audit lines, one per action done.
func adminLines(logs string) int { return strings.Count(logs, `"msg":"admin"`) }

func TestTheAdminMatrix(t *testing.T) {
	for _, id := range sortedKeys(adminMatrix) {
		c := adminMatrix[id]
		for _, who := range matrixCallers {
			t.Run(id+"/"+who.tok, func(t *testing.T) {
				m := newMatrixEnv(t)
				fill := func(s, slug string) string {
					s = strings.ReplaceAll(s, "{own}", slug)
					return strings.ReplaceAll(s, "{tok}", strconv.FormatInt(int64(m.tokBID), 10))
				}
				otherTenant := map[string]string{"only-a": "org-a", "only-b": "org-b"}[who.other]
				beforeOther := ""
				if who.other != "" {
					beforeOther = m.inboxState(otherTenant, who.other)
				}
				audits := adminLines(m.logs.String())
				r := m.call(c.method, fill(c.path, who.own), fill(c.body, who.own), who.tok)
				code, msg := errorOf(r.body)

				switch {
				case !who.admin:
					if r.code != http.StatusForbidden || code != "forbidden" || msg != "this credential cannot use the admin API" {
						t.Errorf("a caller that is not an admin: %d %s", r.code, r.body)
					}
				case c.storeWide && who.tenantScoped:
					if r.code != http.StatusForbidden || code != "forbidden" || !strings.Contains(msg, "tenant-scoped administrator") {
						t.Errorf("a tenant's admin on a store-wide operation: %d %.300s, want 403 forbidden", r.code, r.body)
					}
					if n := adminLines(m.logs.String()); n != audits {
						t.Errorf("the refused operation was done anyway: %d admin lines, had %d", n, audits)
					}
				default:
					if r.code != c.ok {
						t.Errorf("%d %.300s, want %d", r.code, r.body, c.ok)
					}
				}
				if who.other != "" {
					if after := m.inboxState(otherTenant, who.other); after != beforeOther {
						t.Errorf("the other tenant's inbox changed: %s -> %s", beforeOther, after)
					}
				}
				if r.code < 300 && who.tenantScoped {
					for _, leak := range []string{who.other, m.tokBName, "key-" + who.other} {
						if leak != "" && strings.Contains(r.body, leak) {
							t.Errorf("the answer names %q, which is another tenant's: %.300s", leak, r.body)
						}
					}
				}
				if !c.cross || !who.admin || !who.tenantScoped {
					return
				}
				// The same operation on the other tenant's inbox: it does not exist.
				x := m.call(c.method, fill(c.path, who.other), fill(c.body, who.other), who.tok)
				if x.code != http.StatusNotFound {
					t.Errorf("on the other tenant's %s: %d %.300s, want 404", who.other, x.code, x.body)
				}
				if after := m.inboxState(otherTenant, who.other); after != beforeOther {
					t.Errorf("the other tenant's inbox changed: %s -> %s", beforeOther, after)
				}
			})
		}
	}
}

// The operations that do not name an inbox, from a tenant's admin, stay in its tenant:
// it lists its own inboxes only, and what it creates is its own, even with the slug
// another tenant uses.
func TestATenantAdminsListingAndCreationStayInItsTenant(t *testing.T) {
	m := newMatrixEnv(t)
	l := m.call("GET", "/v1/admin/inboxes", "", "admin-a")
	if l.code != 200 || !strings.Contains(l.body, `"only-a"`) || strings.Contains(l.body, "only-b") ||
		strings.Contains(l.body, "home") || strings.Count(l.body, `"slug"`) != 2 {
		t.Errorf("admin-a lists %d %s, want alerts and only-a", l.code, l.body)
	}
	before := m.inboxState("org-b", "only-b")
	if r := m.call("POST", "/v1/admin/inboxes", `{"slug":"only-b","verifier":"none"}`, "admin-a"); r.code != 201 {
		t.Fatalf("admin-a creates only-b: %d %s", r.code, r.body)
	}
	if in, err := m.store.GetInboxBySlug(inbox.WithTenant(t.Context(), "org-a"), "only-b"); err != nil || in.Tenant != "org-a" {
		t.Errorf("admin-a's only-b: %+v %v, want in org-a", in, err)
	}
	if after := m.inboxState("org-b", "only-b"); after != before {
		t.Errorf("org-b's only-b changed: %s -> %s", before, after)
	}
	// The body has no way to name a tenant.
	if r := m.call("POST", "/v1/admin/inboxes", `{"slug":"x","verifier":"none","tenant":"org-b"}`, "admin-a"); r.code != 400 {
		t.Errorf("a tenant in the body: %d %s, want 400", r.code, r.body)
	}
}

// The three ways a tenant's admin could reach other tenants through /v1/admin/tokens
// before tokens were the operator's only.
func TestATenantAdminCannotReachOtherTenantsThroughTokens(t *testing.T) {
	m := newMatrixEnv(t)
	// An all-inboxes token of the operator's, which the listing showed to everyone.
	if r := m.call("POST", "/v1/admin/tokens", `{"name":"operator-wide","all_inboxes":true}`, "operator"); r.code != 201 {
		t.Fatalf("the operator's token: %d %s", r.code, r.body)
	}
	tokens := func() int {
		l, err := m.store.ListTokens(t.Context(), inbox.TokenListParams{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(l)
	}
	have := tokens()

	// 1. Listing.
	for _, path := range []string{"/v1/admin/tokens", "/v1/admin/tokens?inbox=only-a"} {
		r := m.call("GET", path, "", "admin-a")
		if r.code != http.StatusForbidden || strings.Contains(r.body, "operator-wide") || strings.Contains(r.body, m.tokBName) {
			t.Errorf("admin-a GET %s: %d %s, want 403", path, r.code, r.body)
		}
	}
	// 2. Revoking another tenant's token by id.
	r := m.call("DELETE", "/v1/admin/tokens/"+strconv.FormatInt(int64(m.tokBID), 10), "", "admin-a")
	if r.code != http.StatusForbidden {
		t.Errorf("admin-a revokes org-b's token: %d %s, want 403", r.code, r.body)
	}
	if _, err := m.store.GetTokenByHash(t.Context(), m.tokenHash); err != nil {
		t.Errorf("org-b's token is gone: %v", err)
	}
	if rec := m.get("/v1/inboxes/only-b", m.tokB); rec.Code != http.StatusNotFound && rec.Code != http.StatusOK {
		t.Errorf("org-b's token no longer authenticates: %d", rec.Code)
	}
	// 3. Issuing a token that reads every tenant's inboxes.
	r = m.call("POST", "/v1/admin/tokens", `{"name":"escape","all_inboxes":true}`, "admin-a")
	if r.code != http.StatusForbidden {
		t.Errorf("admin-a issues an all_inboxes token: %d %s, want 403", r.code, r.body)
		var made struct{ Token string }
		if json.Unmarshal([]byte(r.body), &made) == nil && made.Token != "" {
			var l struct {
				Inboxes []struct {
					Slug string `json:"slug"`
				} `json:"inboxes"`
			}
			_ = json.Unmarshal(callTool(t, connectMCP(t, m.base, made.Token), "list_inboxes", nil).raw, &l)
			for _, in := range l.Inboxes {
				if in.Slug == "only-b" {
					t.Errorf("and with it, admin-a's token lists org-b's inboxes: %+v", l.Inboxes)
					break
				}
			}
		}
	}
	if r := m.call("POST", "/v1/admin/tokens", `{"name":"mine","inboxes":["only-a"]}`, "admin-a"); r.code != http.StatusForbidden {
		t.Errorf("admin-a issues a token for its own inbox: %d %s, want 403 (tokens are the operator's)", r.code, r.body)
	}
	if n := tokens(); n != have {
		t.Errorf("%d tokens after admin-a's attempts, had %d", n, have)
	}

	// The refusal is logged at WARN with the operation and the principal's type, never
	// the credential.
	var line string
	for _, l := range strings.Split(m.logs.String(), "\n") {
		if strings.Contains(l, "tenant-scoped administrator") && strings.Contains(l, `"op":"createToken"`) {
			line = l
		}
	}
	if !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, "orgPrincipal") {
		t.Errorf("the refusal's log line: %q", line)
	}
	if strings.Contains(m.logs.String(), "Bearer") || strings.Contains(line, "admin-a") {
		t.Errorf("the log has the credential:\n%s", m.logs)
	}
}

// RequireTenant does not change any of this: the operator, an admin in no tenant, is
// still served everything, and a tenant's admin still only its tenant's operations.
func TestRequireTenantKeepsTheOperatorsStoreWideOperations(t *testing.T) {
	m := newMatrixEnv(t, func(o *Options) { o.RequireTenant = true })
	for _, id := range []string{"adminStatus", "backup", "listTokens", "runRetention"} {
		c := adminMatrix[id]
		if r := m.call(c.method, c.path, c.body, "operator"); r.code != c.ok {
			t.Errorf("operator %s: %d %.200s, want %d", id, r.code, r.body, c.ok)
		}
		if r := m.call(c.method, c.path, c.body, "admin-a"); r.code != http.StatusForbidden {
			t.Errorf("admin-a %s: %d %.200s, want 403", id, r.code, r.body)
		}
	}
	if r := m.call("GET", "/v1/admin/inboxes/only-a", "", "admin-a"); r.code != 200 {
		t.Errorf("admin-a shows its inbox: %d %s", r.code, r.body)
	}
	if r := m.call("GET", "/v1/admin/inboxes", "", "plain"); r.code != http.StatusInternalServerError {
		t.Errorf("plain under RequireTenant: %d, want 500", r.code)
	}
	if status, _ := do2(t, "GET", m.base+"/metrics", http.Header{"Authorization": {"Bearer operator"}}, ""); status != 200 {
		t.Errorf("operator /metrics: %d", status)
	}
}

// Every admin operation in the document says whose data it acts on: "tenant" for one
// that a tenant's admin may call (it acts only on that tenant's inboxes), "store" for
// one that is the operator's only. And every one is in the matrix above, with the same
// answer, so a new admin operation cannot be added without deciding and testing it.
func TestEveryAdminOperationIsClassifiedAndInTheMatrix(t *testing.T) {
	e := newEnv(t, withAuth)
	var ids []string
	for _, op := range e.server.spec.Operations() {
		if !op.HasTag("Admin") {
			if _, has := op.Raw()["x-admin-scope"]; has {
				t.Errorf("%s is not an admin operation but has x-admin-scope", op.ID)
			}
			continue
		}
		ids = append(ids, op.ID)
		scope, _ := op.Raw()["x-admin-scope"].(string)
		switch scope {
		case "tenant", "store":
		default:
			t.Errorf("%s %s: x-admin-scope is %q; say \"tenant\" or \"store\"", op.Method, op.Path, scope)
			continue
		}
		c, ok := adminMatrix[op.ID]
		if !ok {
			t.Errorf("%s is not in adminMatrix", op.ID)
			continue
		}
		if c.storeWide != (scope == "store") {
			t.Errorf("%s: the document says %q, the matrix says storeWide=%v", op.ID, scope, c.storeWide)
		}
	}
	for id := range adminMatrix {
		if !slices.Contains(ids, id) {
			t.Errorf("adminMatrix has %s, which is not an admin operation of the document", id)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
