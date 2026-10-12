// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// adminEnv is a server on a socket with the admin API, and a client for it.
type adminEnv struct {
	*agentEnv
	base string
}

func newAdminEnv(t *testing.T, mods ...func(*Options)) *adminEnv {
	t.Helper()
	all := append([]func(*Options){func(o *Options) { o.Notifier = local.New() }}, mods...)
	ae := newAgentEnv2(t, all...)
	ae.tokenA = ae.tokenFor(ae.a)
	return &adminEnv{agentEnv: ae, base: serveOn(t, ae.env)}
}

// call sends an admin request. body may be a string (sent as is), nil, or
// anything else (sent as JSON).
func (a *adminEnv) call(method, path string, body any, tok string) (int, http.Header, []byte) {
	a.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			a.t.Fatal(err)
		}
		rdr = bytes.NewReader(j)
	}
	req, err := http.NewRequestWithContext(a.t.Context(), method, a.base+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	checkAgainstSpec(a.t, method, path, resp.StatusCode, resp.Header, data)
	return resp.StatusCode, resp.Header, data
}

// as admin: the common case.
func (a *adminEnv) admin(method, path string, body any) (int, []byte) {
	a.t.Helper()
	status, _, data := a.call(method, path, body, testAdminToken)
	return status, data
}

func (a *adminEnv) mustAdmin(method, path string, body any, want int) []byte {
	a.t.Helper()
	status, data := a.admin(method, path, body)
	if status != want {
		a.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, data)
	}
	return data
}

type createdJSON struct {
	Inbox      viewJSON `json:"inbox"`
	AgentToken string   `json:"agent_token"`
	Warnings   []string `json:"warnings"`
}

type viewJSON struct {
	IngestKey         string          `json:"ingest_key"`
	ID                int64           `json:"id"`
	Slug              string          `json:"slug"`
	Name              string          `json:"name"`
	Version           int64           `json:"version"`
	Verifier          string          `json:"verifier"`
	VerifierConfig    json.RawMessage `json:"verifier_config"`
	HasVerifierSecret bool            `json:"has_verifier_secret"`
	OnVerifyFail      string          `json:"on_verify_fail"`
	MaxBodyBytes      int64           `json:"max_body_bytes"`
	RetentionDays     int             `json:"retention_days"`
	RetentionMaxCount int64           `json:"retention_max_count"`
	EventTypePointer  string          `json:"event_type_pointer"`
	Disabled          bool            `json:"disabled"`
	LastSeq           int64           `json:"last_seq"`
	MessageCount      *int64          `json:"message_count"`
	IngestPath        string          `json:"ingest_path"`
	IngestURL         string          `json:"ingest_url"`
	Warnings          []string        `json:"warnings"`
}

func unjson[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return v
}

// ---- access ----

func TestAdminAPIRequiresTheAdminToken(t *testing.T) {
	a := newAdminEnv(t)
	routes := []struct{ method, path string }{
		{"POST", "/v1/admin/inboxes"}, {"GET", "/v1/admin/inboxes"}, {"GET", "/v1/admin/inboxes/agent-a"},
		{"PATCH", "/v1/admin/inboxes/agent-a"}, {"DELETE", "/v1/admin/inboxes/agent-a"},
		{"PUT", "/v1/admin/inboxes/agent-a/secret"}, {"POST", "/v1/admin/inboxes/agent-a/rotate-ingest-key"},
		{"POST", "/v1/admin/tokens"}, {"GET", "/v1/admin/tokens"}, {"DELETE", "/v1/admin/tokens/1"},
		{"POST", "/v1/admin/retention/run"}, {"GET", "/v1/admin/backup"}, {"GET", "/metrics"},
	}
	for _, r := range routes {
		for name, tok := range map[string]string{"no token": "", "bad token": "nope", "an agent token": a.tokenA} {
			status, hdr, body := a.call(r.method, r.path, nil, tok)
			want := http.StatusUnauthorized
			if tok == a.tokenA {
				want = http.StatusForbidden
			}
			if status != want {
				t.Errorf("%s %s with %s: status %d, want %d (%s)", r.method, r.path, name, status, want, body)
			}
			if want == 401 && hdr.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s: a 401 without WWW-Authenticate", r.method, r.path)
			}
			if hdr.Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: Cache-Control %q", r.method, r.path, hdr.Get("Cache-Control"))
			}
		}
	}
	// Nothing was done by any of them.
	if list, _ := a.store.ListInboxes(t.Context(), inbox.ListParams{}); len(list) != 1 {
		t.Errorf("a refused request changed the inboxes: %d", len(list))
	}
}

func TestAdminAPIWrongMethodIsJSON405(t *testing.T) {
	a := newAdminEnv(t)
	for path, allow := range map[string]string{
		"/v1/admin/inboxes": "GET, POST", "/v1/admin/inboxes/agent-a": "DELETE, GET, PATCH", "/v1/admin/inboxes/agent-a/secret": "PUT",
		"/v1/admin/inboxes/agent-a/rotate-ingest-key": "POST", "/v1/admin/tokens": "GET, POST", "/v1/admin/tokens/1": "DELETE", "/v1/admin/retention/run": "POST", "/v1/admin/backup": "GET", "/metrics": "GET",
	} {
		status, hdr, body := a.call("OPTIONS", path, nil, testAdminToken)
		if status != http.StatusMethodNotAllowed || hdr.Get("Allow") != allow || errorCodeOfBody(t, body) != "method_not_allowed" {
			t.Errorf("%s: status %d, Allow %q, %s", path, status, hdr.Get("Allow"), body)
		}
	}
	// An agent token is refused before the method is looked at.
	if status, _, _ := a.call("OPTIONS", "/v1/admin/inboxes", nil, a.tokenA); status != http.StatusForbidden {
		t.Errorf("an agent token with a wrong method got %d", status)
	}
}

func errorCodeOfBody(t *testing.T, body []byte) string {
	t.Helper()
	var e struct{ Error struct{ Code string } }
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not a JSON error: %q", body)
	}
	return e.Error.Code
}

func TestAdminAPIIsOffWithoutAnAuthenticator(t *testing.T) {
	e := newEnv(t)
	base := serveOn(t, e)
	status, _ := do2(t, http.MethodGet, base+"/v1/admin/inboxes", http.Header{"Authorization": {"Bearer " + testAdminToken}}, "")
	if status != http.StatusNotFound {
		t.Errorf("status %d without an Authenticator, want 404", status)
	}
}

// ---- the lifecycle ----

func TestCreateShowListUpdateDelete(t *testing.T) {
	a := newAdminEnv(t, func(o *Options) { o.PublicBaseURL = "https://hooks.example.com/" })

	status, hdr, body := a.call("POST", "/v1/admin/inboxes", map[string]any{
		"slug": "billing", "name": "Billing", "verifier": "github", "verifier_secret": "gh-secret",
		"retention_days": 30, "retention_max_count": 5000, "event_type_pointer": "/type", "max_body_bytes": 2048,
	}, testAdminToken)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	created := unjson[createdJSON](t, body)
	if hdr.Get("Location") != "/v1/admin/inboxes/billing" {
		t.Errorf("Location = %q", hdr.Get("Location"))
	}
	v := created.Inbox
	if v.Slug != "billing" || v.Name != "Billing" || v.Verifier != "github" || !v.HasVerifierSecret || v.RetentionDays != 30 ||
		v.RetentionMaxCount != 5000 || v.EventTypePointer != "/type" || v.MaxBodyBytes != 2048 || v.OnVerifyFail != "reject" || v.Version != 1 {
		t.Errorf("created inbox = %+v", v)
	}
	// The ingest URL is not the slug: its key is random, and is what a sender needs.
	if len(v.IngestKey) < 20 || v.IngestKey == "billing" || v.IngestPath != "/in/"+v.IngestKey || v.IngestURL != "https://hooks.example.com"+v.IngestPath {
		t.Errorf("ingest key %q, path %q, url %q", v.IngestKey, v.IngestPath, v.IngestURL)
	}
	ingest := a.base + v.IngestPath
	if !strings.HasPrefix(created.AgentToken, "hki_") || len(created.Warnings) != 0 {
		t.Errorf("token %q, warnings %v", created.AgentToken, created.Warnings)
	}

	// The token works for the read API, and the inbox works for deliveries.
	if status, _ := do2(t, "GET", a.base+"/v1/inboxes/billing", http.Header{"Authorization": {"Bearer " + created.AgentToken}}, ""); status != 200 {
		t.Errorf("the new token cannot read its inbox: %d", status)
	}
	payload := `{"type":"push"}`
	if status, body := do2(t, "POST", ingest, http.Header{"X-Hub-Signature-256": {githubSig("gh-secret", payload)}}, payload); status != 200 {
		t.Errorf("a signed delivery to the new inbox: %d %s", status, body)
	}

	// Show.
	shown := unjson[viewJSON](t, a.mustAdmin("GET", "/v1/admin/inboxes/billing", nil, 200))
	if shown.MessageCount == nil || *shown.MessageCount != 1 || shown.LastSeq != 1 || shown.IngestURL != v.IngestURL {
		t.Errorf("shown = %+v", shown)
	}

	// Update.
	patched := unjson[viewJSON](t, a.mustAdmin("PATCH", "/v1/admin/inboxes/billing", map[string]any{
		"name": "Billing events", "retention_days": 0, "disabled": true, "on_verify_fail": "store",
	}, 200))
	if patched.Name != "Billing events" || patched.RetentionDays != 0 || !patched.Disabled || patched.OnVerifyFail != "store" ||
		patched.RetentionMaxCount != 5000 || patched.Version != 2 || !patched.HasVerifierSecret {
		t.Errorf("patched = %+v", patched)
	}
	if status, _ := do2(t, "POST", ingest, http.Header{"X-Hub-Signature-256": {githubSig("gh-secret", payload)}}, payload); status != 404 {
		t.Errorf("a disabled inbox accepted a delivery: %d", status)
	}
	a.mustAdmin("PATCH", "/v1/admin/inboxes/billing", map[string]any{"disabled": false}, 200)

	// List, in pages.
	for _, slug := range []string{"second-inbox", "third-inbox", "fourth-inbox"} {
		a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": slug, "verifier": "none"}, 201)
	}
	var seen []string
	path := "/v1/admin/inboxes?limit=2"
	for pages := 0; path != "" && pages < 10; pages++ {
		page := unjson[struct {
			Inboxes []viewJSON `json:"inboxes"`
			Next    *string    `json:"next"`
		}](t, a.mustAdmin("GET", path, nil, 200))
		for _, in := range page.Inboxes {
			seen = append(seen, in.Slug)
		}
		path = ""
		if page.Next != nil {
			path = *page.Next
		}
	}
	if strings.Join(seen, ",") != "agent-a,billing,second-inbox,third-inbox,fourth-inbox" {
		t.Errorf("listed %v", seen)
	}

	// Delete.
	if status, body := a.admin("DELETE", "/v1/admin/inboxes/billing", nil); status != http.StatusNoContent || len(body) != 0 {
		t.Errorf("delete: %d %q", status, body)
	}
	a.mustAdmin("GET", "/v1/admin/inboxes/billing", nil, 404)
	a.mustAdmin("DELETE", "/v1/admin/inboxes/billing", nil, 404)
	if status, _ := do2(t, "POST", a.base+"/in/billing", http.Header{}, payload); status != 404 {
		t.Errorf("a deleted inbox accepted a delivery: %d", status)
	}
	if status, _ := do2(t, "GET", a.base+"/v1/inboxes/billing", http.Header{"Authorization": {"Bearer " + created.AgentToken}}, ""); status != 404 && status != 401 {
		t.Errorf("a deleted inbox is still readable: %d", status)
	}
}

func TestCreateValidatesBeforeStoring(t *testing.T) {
	a := newAdminEnv(t)
	for name, c := range map[string]struct {
		body any
		want string // the message must mention this
	}{
		"bad slug":                {map[string]any{"slug": "No"}, "slug"},
		"missing slug":            {map[string]any{"name": "x"}, "slug"},
		"unknown verifier":        {map[string]any{"slug": "val-a", "verifier": "nope"}, "unknown verifier"},
		"verifier config invalid": {map[string]any{"slug": "val-b", "verifier": "hmac", "verifier_config": map[string]any{}}, "header"},
		"verifier config typo":    {map[string]any{"slug": "val-c", "verifier": "stripe", "verifier_config": map[string]any{"toleranse_seconds": 5}}, "toleranse"},
		"bad on_verify_fail":      {map[string]any{"slug": "val-d", "on_verify_fail": "maybe"}, "on_verify_fail"},
		"huge body limit":         {map[string]any{"slug": "val-e", "max_body_bytes": 1 << 40}, "max_body_bytes"},
		"negative retention":      {map[string]any{"slug": "val-f", "retention_days": -1}, "retention_days"},
		"bad pointer":             {map[string]any{"slug": "val-g", "event_type_pointer": "type"}, "event_type_pointer"},
		"unknown field":           {map[string]any{"slug": "val-h", "agent_token_hash": "x"}, "agent_token_hash"},
		"secret too long":         {map[string]any{"slug": "val-i", "verifier": "token", "verifier_secret": strings.Repeat("s", 5000)}, "secret"},
		"not an object":           {`["x"]`, "request body"},
		"trailing data":           {`{"slug":"val-j"} {"slug":"val-k"}`, "single JSON value"},
	} {
		status, body := a.admin("POST", "/v1/admin/inboxes", c.body)
		if status != http.StatusBadRequest || !strings.Contains(string(body), c.want) {
			t.Errorf("%s: status %d, %s (want a 400 mentioning %q)", name, status, body, c.want)
		}
		if strings.Contains(string(body), "sssss") {
			t.Errorf("%s: the response echoes the secret", name)
		}
	}
	if list, _ := a.store.ListInboxes(t.Context(), inbox.ListParams{}); len(list) != 1 {
		t.Errorf("a rejected create left %d inboxes behind", len(list)-1)
	}

	a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "dup-inbox", "verifier": "none"}, 201)
	status, body := a.admin("POST", "/v1/admin/inboxes", map[string]any{"slug": "dup-inbox", "verifier": "none"})
	if status != http.StatusConflict || errorCodeOfBody(t, body) != "conflict" {
		t.Errorf("a duplicate slug: %d %s", status, body)
	}
}

func TestRequestBodiesAreChecked(t *testing.T) {
	a := newAdminEnv(t)
	status, _, body := a.call("POST", "/v1/admin/inboxes", "slug=x", testAdminToken) // JSON content type, form body
	if status != http.StatusBadRequest {
		t.Errorf("a non-JSON body: %d %s", status, body)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", a.base+"/v1/admin/inboxes", strings.NewReader(`{"slug":"abc-def"}`))
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type: %d", resp.StatusCode)
	}
	status, _ = a.admin("POST", "/v1/admin/inboxes", `{"slug":"abc-def","name":"`+strings.Repeat("n", 100<<10)+`"}`)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("a 100 KiB body: %d", status)
	}
}

func TestAWarningSaysWhenAVerifierHasNoSecret(t *testing.T) {
	a := newAdminEnv(t)
	c := unjson[createdJSON](t, a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": "needs-secret", "verifier": "github"}, 201))
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "needs a secret") || c.Inbox.HasVerifierSecret {
		t.Errorf("warnings %v, has secret %v", c.Warnings, c.Inbox.HasVerifierSecret)
	}
	// And setting the secret makes deliveries verify.
	payload := `{"x":1}`
	hdr := http.Header{"X-Hub-Signature-256": {githubSig("late-secret", payload)}}
	ingest := a.base + c.Inbox.IngestPath
	if status, _ := do2(t, "POST", ingest, hdr, payload); status == 200 {
		t.Error("a delivery was accepted before any secret was set")
	}
	v := unjson[viewJSON](t, a.mustAdmin("PUT", "/v1/admin/inboxes/needs-secret/secret", map[string]any{"secret": "late-secret"}, 200))
	if !v.HasVerifierSecret {
		t.Error("the inbox does not say it has a secret")
	}
	if status, body := do2(t, "POST", ingest, hdr, payload); status != 200 {
		t.Errorf("a signed delivery after setting the secret: %d %s", status, body)
	}
	// Rotating the secret replaces it.
	a.mustAdmin("PUT", "/v1/admin/inboxes/needs-secret/secret", map[string]any{"secret": "newer-secret"}, 200)
	if status, _ := do2(t, "POST", ingest, http.Header{"X-Hub-Signature-256": {githubSig("late-secret", payload+" ")}}, payload+" "); status == 200 {
		t.Error("the old secret still verifies")
	}
	if status, _ := a.admin("PUT", "/v1/admin/inboxes/needs-secret/secret", map[string]any{"secret": ""}); status != 400 {
		t.Errorf("an empty secret: %d", status)
	}
	if status, _ := a.admin("PUT", "/v1/admin/inboxes/no-such-inbox/secret", map[string]any{"secret": "x"}); status != 404 {
		t.Errorf("a secret for a missing inbox: %d", status)
	}
}

func TestPatchValidatesTheResultingVerifier(t *testing.T) {
	a := newAdminEnv(t)
	a.mustAdmin("PATCH", "/v1/admin/inboxes/agent-a", map[string]any{"verifier": "token"}, 200) // one that checks its configuration
	for name, p := range map[string]map[string]any{
		"unknown verifier":  {"verifier": "nope"},
		"hmac needs header": {"verifier": "hmac"},
		"bad config":        {"verifier_config": map[string]any{"header": 5}},
		"nothing":           {},
		"slug":              {"slug": "other-name"},
		"token hash":        {"agent_token_hash": "x"},
		"bad retention":     {"retention_max_count": -3},
	} {
		if status, body := a.admin("PATCH", "/v1/admin/inboxes/agent-a", p); status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	a.mustAdmin("PATCH", "/v1/admin/inboxes/agent-a", map[string]any{"verifier": "hmac", "verifier_config": map[string]any{"header": "X-Sig"}}, 200)
	if status, _ := a.admin("PATCH", "/v1/admin/inboxes/no-such-inbox", map[string]any{"name": "x"}); status != 404 {
		t.Errorf("patching a missing inbox: %d", status)
	}
}

type tokenCreatedJSON struct {
	Record struct {
		ID         int64    `json:"id"`
		Name       string   `json:"name"`
		AllInboxes bool     `json:"all_inboxes"`
		Inboxes    []string `json:"inboxes"`
	} `json:"record"`
	Token string `json:"token"`
}

func TestTokensAreIssuedScopedAndRevoked(t *testing.T) {
	a := newAdminEnv(t)
	a.newInbox("agent-b", "trusted", "")
	read := func(tok, slug string) int {
		status, _ := do2(t, "GET", a.base+"/v1/inboxes/"+slug, http.Header{"Authorization": {"Bearer " + tok}}, "")
		return status
	}
	if read(a.tokenA, "agent-a") != 200 {
		t.Fatal("the starting token does not work")
	}
	// A second token for the same inbox, so the first can be retired with no gap.
	second := unjson[tokenCreatedJSON](t, a.mustAdmin("POST", "/v1/admin/tokens", map[string]any{"name": "next", "inboxes": []string{"agent-a"}}, 201))
	if second.Token == "" || second.Token == a.tokenA || second.Record.Name != "next" || second.Record.AllInboxes ||
		len(second.Record.Inboxes) != 1 || second.Record.Inboxes[0] != "agent-a" {
		t.Fatalf("created token %+v", second)
	}
	if read(second.Token, "agent-a") != 200 || read(a.tokenA, "agent-a") != 200 {
		t.Error("both tokens should work until the old one is revoked")
	}
	if read(second.Token, "agent-b") != 404 && read(second.Token, "agent-b") != 403 {
		t.Errorf("a token scoped to agent-a reads agent-b: %d", read(second.Token, "agent-b"))
	}

	list := unjson[struct {
		Tokens []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"tokens"`
	}](t, a.mustAdmin("GET", "/v1/admin/tokens?inbox=agent-a", nil, 200))
	var firstID int64
	for _, tk := range list.Tokens {
		if tk.Name == "default" {
			firstID = tk.ID
		}
	}
	if len(list.Tokens) < 2 {
		t.Fatalf("tokens for agent-a: %+v", list.Tokens)
	}

	// An all-inboxes token reads both inboxes and still cannot administer.
	all := unjson[tokenCreatedJSON](t, a.mustAdmin("POST", "/v1/admin/tokens", map[string]any{"name": "reader", "all_inboxes": true}, 201))
	if read(all.Token, "agent-a") != 200 || read(all.Token, "agent-b") != 200 {
		t.Error("an all-inboxes token should read every inbox")
	}
	if status, _, _ := a.call("GET", "/v1/admin/inboxes", nil, all.Token); status != http.StatusForbidden {
		t.Errorf("an all-inboxes token on the admin API: %d, want 403", status)
	}

	// Revoking the first leaves the second.
	if firstID == 0 {
		firstID = list.Tokens[0].ID
	}
	a.mustAdmin("DELETE", "/v1/admin/tokens/"+strconv.FormatInt(firstID, 10), nil, 204)
	if status, _ := a.admin("DELETE", "/v1/admin/tokens/"+strconv.FormatInt(firstID, 10), nil); status != 404 {
		t.Errorf("revoking twice: %d", status)
	}
	if read(a.tokenA, "agent-a") != 401 {
		t.Errorf("the revoked token still works: %d", read(a.tokenA, "agent-a"))
	}
	if read(second.Token, "agent-a") != 200 {
		t.Error("the other token stopped working")
	}

	for name, body := range map[string]map[string]any{
		"an unknown inbox":      {"name": "x", "inboxes": []string{"no-such-inbox"}},
		"neither scope":         {"name": "x"},
		"both scopes":           {"name": "x", "inboxes": []string{"agent-a"}, "all_inboxes": true},
		"a nameless token":      {"name": "", "all_inboxes": true},
		"an unknown field":      {"name": "x", "all_inboxes": true, "extra": 1},
		"no name at all":        {"all_inboxes": true},
		"a repeated inbox name": {"name": "x", "inboxes": []string{"agent-a", "agent-a"}},
	} {
		status, _ := a.admin("POST", "/v1/admin/tokens", body)
		want := http.StatusBadRequest
		if name == "an unknown inbox" {
			want = http.StatusNotFound
		}
		if status != want {
			t.Errorf("%s: %d, want %d", name, status, want)
		}
	}
}

// Two admins changing an inbox at once must not lose either change.
func TestConcurrentPatchesAreBothApplied(t *testing.T) {
	a := newAdminEnv(t)
	start := unjson[viewJSON](t, a.mustAdmin("GET", "/v1/admin/inboxes/agent-a", nil, 200)).Version
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			field := map[string]any{"retention_days": i + 1}
			if i%2 == 1 {
				field = map[string]any{"retention_max_count": i + 100}
			}
			if status, body := a.admin("PATCH", "/v1/admin/inboxes/agent-a", field); status != 200 {
				t.Errorf("patch %d: %d %s", i, status, body)
			}
		})
	}
	wg.Wait()
	v := unjson[viewJSON](t, a.mustAdmin("GET", "/v1/admin/inboxes/agent-a", nil, 200))
	if v.Version != start+8 || v.RetentionDays == 0 || v.RetentionMaxCount == 0 {
		t.Errorf("after 8 patches from version %d: version %d, days %d, count %d", start, v.Version, v.RetentionDays, v.RetentionMaxCount)
	}
}

// ---- nothing secret leaves ----

func TestNoResponseOrLogContainsASecret(t *testing.T) {
	a := newAdminEnv(t, func(o *Options) { o.PublicBaseURL = "https://hooks.example.com" })
	const secret = "TOP-SECRET-verifier-value-91827" //nolint:gosec // a fake value; the test proves it never leaves
	var all bytes.Buffer
	record := func(status int, hdr http.Header, body []byte) {
		fmt.Fprintf(&all, "%d %v\n%s\n", status, hdr, body)
	}
	call := func(method, path string, body any) []byte {
		status, hdr, data := a.call(method, path, body, testAdminToken)
		record(status, hdr, data)
		return data
	}

	c := unjson[createdJSON](t, call("POST", "/v1/admin/inboxes", map[string]any{"slug": "leaky", "verifier": "github", "verifier_secret": secret}))
	call("GET", "/v1/admin/inboxes", nil)
	call("GET", "/v1/admin/inboxes/leaky", nil)
	call("PATCH", "/v1/admin/inboxes/leaky", map[string]any{"name": "Leaky"})
	call("PUT", "/v1/admin/inboxes/leaky/secret", map[string]any{"secret": secret + "-2"})
	rot := unjson[tokenCreatedJSON](t, call("POST", "/v1/admin/tokens", map[string]any{"name": "second", "inboxes": []string{"leaky"}}))
	call("GET", "/v1/admin/tokens", nil)
	call("POST", "/v1/admin/inboxes", map[string]any{"slug": "leaky", "verifier_secret": secret}) // a conflict
	call("POST", "/v1/admin/inboxes", map[string]any{"slug": "x", "verifier_secret": secret})     // invalid
	call("GET", "/v1/admin/inboxes/nope", nil)
	call("POST", "/v1/admin/retention/run", nil)
	body := `{"a":1}`
	status, text := do2(t, "POST", a.base+"/in/leaky", http.Header{"X-Hub-Signature-256": {githubSig("wrong", body)}}, body)
	fmt.Fprintf(&all, "%d %s\n", status, text)
	// (The backup holds the sealed secret by design, so it is not among the responses checked.)

	// Everything a client was shown, and everything logged.
	in, err := a.store.GetInboxBySlug(t.Context(), "leaky")
	if err != nil {
		t.Fatal(err)
	}
	hash, sealed := token.Hash(c.AgentToken), []byte(in.VerifierSecret)
	shown := all.String() + "\n" + a.logs.String()
	forbidden := map[string]string{
		"the verifier secret": secret, "the sealed secret": string(sealed), "its hex": hex.EncodeToString(sealed),
		"its base64": base64.StdEncoding.EncodeToString(sealed), "the token hash": string(hash),
		"its hex form": hex.EncodeToString(hash), "its base64 form": base64.StdEncoding.EncodeToString(hash),
		"its url-safe base64": base64.RawURLEncoding.EncodeToString(hash), "the first token": c.AgentToken[4:],
	}
	delete(forbidden, "the first token") // checked separately: it was shown once, in the create response
	for what, needle := range forbidden {
		if needle != "" && strings.Contains(shown, needle) {
			t.Errorf("%s appears in a response or the logs", what)
		}
	}
	// A token is shown in exactly the response that created it, and nowhere else.
	for name, tok := range map[string]string{"created": c.AgentToken, "second": rot.Token} {
		if got := strings.Count(shown, tok); got != 1 {
			t.Errorf("the %s token appears %d times in responses and logs, want exactly 1", name, got)
		}
	}
	if strings.Contains(a.logs.String(), c.AgentToken) || strings.Contains(a.logs.String(), rot.Token) {
		t.Error("a token was logged")
	}
	if !strings.Contains(a.logs.String(), `"msg":"admin"`) {
		t.Error("admin actions are not logged")
	}
}

// ---- retention ----

// clockedStore opens a Store whose clock the test moves.
func clockedStore(t *testing.T, now *atomic.Int64) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(t.Context(), t.TempDir(), sqlite.WithClock(func() time.Time { return time.Unix(0, now.Load()).UTC() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRunningRetentionByAgeByCountAndBoth(t *testing.T) {
	day0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(day0.UnixNano())
	a := newAdminEnv(t,
		func(o *Options) { o.Store = clockedStore(t, &clock) },
		func(o *Options) { o.Authenticator = nil; withAuth(o) },
		func(o *Options) { o.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() } },
	)
	mk := func(slug string, days int, count int64) {
		a.mustAdmin("POST", "/v1/admin/inboxes", map[string]any{"slug": slug, "ingest_key": slug, "verifier": "none", "retention_days": days, "retention_max_count": count}, 201)
	}
	mk("by-age", 2, 0)
	mk("by-count", 0, 3)
	mk("by-both", 2, 4)
	mk("keeps-all", 0, 0)
	// Ten messages per inbox, one per day, from day 0 to day 9.
	for d := 0; d < 10; d++ {
		clock.Store(day0.AddDate(0, 0, d).UnixNano())
		for _, slug := range []string{"by-age", "by-count", "by-both", "keeps-all"} {
			if status, _ := do2(t, "POST", a.base+"/in/"+slug, http.Header{}, fmt.Sprintf("m%d", d)); status != 200 {
				t.Fatalf("ingest %s: %d", slug, status)
			}
		}
	}
	// It is the end of day 9, and the sweep has not run.
	clock.Store(day0.AddDate(0, 0, 9).UnixNano())
	count := func(slug string) int64 {
		return *unjson[viewJSON](t, a.mustAdmin("GET", "/v1/admin/inboxes/"+slug, nil, 200)).MessageCount
	}
	for _, slug := range []string{"by-age", "by-count", "by-both", "keeps-all"} {
		if count(slug) != 10 {
			t.Fatalf("%s holds %d messages before the sweep", slug, count(slug))
		}
	}

	got := unjson[map[string]int64](t, a.mustAdmin("POST", "/v1/admin/retention/run", nil, 200))
	// By age (2 days): messages older than two days at day 9 are days 0..6; days 7, 8, 9 stay.
	// By count (3): the newest 3 stay. Both (2 days, 4 messages): the stricter of the two, which is 3 (age) here.
	want := map[string]int64{"by-age": 3, "by-count": 3, "by-both": 3, "keeps-all": 10}
	for slug, w := range want {
		if c := count(slug); c != w {
			t.Errorf("%s holds %d messages after the sweep, want %d", slug, c, w)
		}
	}
	if got["deleted"] != 7+7+7 {
		t.Errorf("deleted = %d, want 21", got["deleted"])
	}
	// And it is idempotent.
	if again := unjson[map[string]int64](t, a.mustAdmin("POST", "/v1/admin/retention/run", nil, 200)); again["deleted"] != 0 {
		t.Errorf("a second sweep deleted %d", again["deleted"])
	}

	// Both rules together: the count rule wins when it is the stricter one.
	a.mustAdmin("PATCH", "/v1/admin/inboxes/by-both", map[string]any{"retention_days": 30, "retention_max_count": 2}, 200)
	a.mustAdmin("POST", "/v1/admin/retention/run", nil, 200)
	if c := count("by-both"); c != 2 {
		t.Errorf("by-both holds %d after tightening the count, want 2", c)
	}
}

// blockedPrune holds Prune until released.
type blockedPrune struct {
	inbox.Store
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (b *blockedPrune) Prune(ctx context.Context, now time.Time) (int64, error) {
	b.calls.Add(1)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return b.Store.Prune(ctx, now)
}

func TestOnlyOneRetentionSweepRunsAtATime(t *testing.T) {
	bp := &blockedPrune{entered: make(chan struct{}, 4), release: make(chan struct{})}
	a := newAdminEnv(t, func(o *Options) { bp.Store = o.Store; o.Store = bp; o.RetentionInterval = -1 })
	done := make(chan int, 1)
	go func() { s, _ := a.admin("POST", "/v1/admin/retention/run", nil); done <- s }()
	<-bp.entered
	status, body := a.admin("POST", "/v1/admin/retention/run", nil)
	if status != http.StatusConflict || errorCodeOfBody(t, body) != "busy" {
		t.Errorf("a second sweep while one runs: %d %s", status, body)
	}
	close(bp.release)
	if s := <-done; s != 200 {
		t.Errorf("the first sweep: %d", s)
	}
	if status, _ := a.admin("POST", "/v1/admin/retention/run", nil); status != 200 {
		t.Errorf("a sweep after the first finished: %d", status)
	}
}

// ---- the schedule ----

func TestTheScheduledSweepRunsAfterListeningAndStopsWithTheServer(t *testing.T) {
	bp := &blockedPrune{entered: make(chan struct{}, 16), release: make(chan struct{})}
	close(bp.release) // sweeps do not block in this test
	e := newAgentEnv2(t, func(o *Options) { bp.Store = o.Store; o.Store = bp; o.RetentionInterval = 100 * time.Millisecond })

	l, err := newListener(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- e.server.Serve(ctx, l) }()

	waitFor(t, "the first sweep", func() bool { return bp.calls.Load() >= 1 })
	waitFor(t, "a scheduled sweep", func() bool { return bp.calls.Load() >= 3 })
	stop()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	after := bp.calls.Load()
	time.Sleep(350 * time.Millisecond)
	if bp.calls.Load() != after {
		t.Errorf("sweeps carried on after Serve returned (%d -> %d)", after, bp.calls.Load())
	}
}

func TestAStartupSweepNeverDelaysListening(t *testing.T) {
	bp := &blockedPrune{entered: make(chan struct{}, 4), release: make(chan struct{})}
	e := newAgentEnv2(t, func(o *Options) { bp.Store = o.Store; o.Store = bp })
	base := serveOn(t, e.env)
	<-bp.entered // the sweep has started and is stuck
	if status, _ := do2(t, "GET", base+"/healthz", http.Header{}, ""); status != 200 {
		t.Errorf("healthz while the first sweep is running: %d", status)
	}
	if status, _ := do2(t, "POST", base+"/in/agent-a", http.Header{}, "x"); status != 200 {
		t.Errorf("an ingest while the first sweep is running: %d", status)
	}
	close(bp.release)
}

func TestShutdownDoesNotWaitForAStuckSweepForever(t *testing.T) {
	bp := &blockedPrune{entered: make(chan struct{}, 4), release: make(chan struct{})}
	e := newAgentEnv2(t, func(o *Options) { bp.Store = o.Store; o.Store = bp; o.ShutdownTimeout = 2 * time.Second })
	l, err := newListener(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- e.server.Serve(ctx, l) }()
	<-bp.entered
	start := time.Now()
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("shutdown took %v with a sweep running", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve never returned: it is waiting on a sweep that has been told to stop")
	}
}

// ---- backup ----

func TestBackupStreamsAConsistentCopy(t *testing.T) {
	a := newAdminEnv(t)
	dataDir := a.dataDir
	for i := 0; i < 40; i++ {
		a.post("/in/agent-a", fmt.Sprintf(`{"n":%d}`, i))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { // ingest carries on during the backup
		for {
			select {
			case <-stop:
				return
			default:
				a.post("/in/agent-a", `{"live":true}`)
			}
		}
	})

	status, hdr, data := a.call("GET", "/v1/admin/backup", nil, testAdminToken)
	close(stop)
	wg.Wait()
	if status != 200 {
		t.Fatalf("backup: %d %s", status, data)
	}
	if hdr.Get("Content-Type") != "application/vnd.sqlite3" || !strings.HasPrefix(hdr.Get("Content-Disposition"), `attachment; filename="hookinlet-`) ||
		hdr.Get("Content-Length") != fmt.Sprint(len(data)) || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v (body %d bytes)", hdr, len(data))
	}
	if !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		t.Fatal("the body is not a SQLite database")
	}

	// It passes an integrity check and holds a consistent history.
	copyPath := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+copyPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		t.Errorf("integrity_check = %q, %v", result, err)
	}
	var n, maxSeq, last int64
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*), MAX(seq) FROM messages`).Scan(&n, &maxSeq); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT last_seq FROM inboxes WHERE slug = 'agent-a'`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if n < 40 || n != maxSeq || n != last {
		t.Errorf("the copy holds %d messages, max seq %d, last_seq %d", n, maxSeq, last)
	}
	// No temporary copy is left in the data directory.
	if left, _ := filepath.Glob(filepath.Join(dataDir, "backup-*")); len(left) != 0 {
		t.Errorf("left behind in the data directory: %v", left)
	}
}

// storeOnly hides every optional capability of the store inside it.
type storeOnly struct{ inbox.Store }

func TestBackupIsNotImplementedWithoutTheCapability(t *testing.T) {
	a := newAdminEnv(t, func(o *Options) { o.Store = storeOnly{o.Store} })
	status, body := a.admin("GET", "/v1/admin/backup", nil)
	if status != http.StatusNotImplemented || errorCodeOfBody(t, body) != "not_implemented" {
		t.Errorf("status %d %s", status, body)
	}
}

type slowBackup struct {
	inbox.Store
	entered chan struct{}
	release chan struct{}
}

func (s slowBackup) Backup(ctx context.Context) (io.ReadCloser, int64, error) {
	s.entered <- struct{}{}
	<-s.release
	return s.Store.(inbox.Backupper).Backup(ctx)
}

func TestOnlyOneBackupRunsAtATime(t *testing.T) {
	sb := slowBackup{entered: make(chan struct{}, 2), release: make(chan struct{})}
	a := newAdminEnv(t, func(o *Options) { sb.Store = o.Store; o.Store = sb })
	first := make(chan int, 1)
	go func() { s, _ := a.admin("GET", "/v1/admin/backup", nil); first <- s }()
	<-sb.entered
	if status, body := a.admin("GET", "/v1/admin/backup", nil); status != http.StatusConflict || errorCodeOfBody(t, body) != "busy" {
		t.Errorf("a second backup: %d %s", status, body)
	}
	close(sb.release)
	if s := <-first; s != 200 {
		t.Errorf("the first backup: %d", s)
	}
}

// ---- ingest URLs ----

func TestIngestURLsComeFromTheRequestWhenNoBaseIsConfigured(t *testing.T) {
	a := newAdminEnv(t)
	show := func(hdr http.Header) viewJSON {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", a.base+"/v1/admin/inboxes/agent-a", nil)
		req.Header.Set("Authorization", "Bearer "+testAdminToken)
		for k, v := range hdr {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return unjson[viewJSON](t, data)
	}
	host := strings.TrimPrefix(a.base, "http://")
	if got := show(nil).IngestURL; got != "http://"+host+"/in/agent-a" {
		t.Errorf("ingest url = %q", got)
	}
	// A header from an address that is not a trusted proxy is ignored.
	if got := show(http.Header{"X-Forwarded-Proto": {"https"}}).IngestURL; got != "http://"+host+"/in/agent-a" {
		t.Errorf("an untrusted X-Forwarded-Proto changed the url to %q", got)
	}
}

// slowToStop ignores cancellation for a while, as a prune in the middle of a
// batch does.
type slowToStop struct {
	inbox.Store
	entered  chan struct{}
	finished atomic.Bool
}

func (s *slowToStop) Prune(ctx context.Context, now time.Time) (int64, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	time.Sleep(300 * time.Millisecond) // finishing the batch it was in
	s.finished.Store(true)
	return 0, ctx.Err()
}

// The caller closes the Store as soon as Serve returns, so Serve must not return
// while a sweep is still using it.
func TestServeReturnsOnlyAfterTheSweepHasStopped(t *testing.T) {
	st := &slowToStop{entered: make(chan struct{}, 1)}
	e := newAgentEnv2(t, func(o *Options) { st.Store = o.Store; o.Store = st; o.ShutdownTimeout = 5 * time.Second })
	l, err := newListener(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- e.server.Serve(ctx, l) }()
	<-st.entered
	stop()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if !st.finished.Load() {
		t.Error("Serve returned while the sweep was still running against the Store")
	}
}

// A client that goes away halfway through a download must not leave a copy of
// the database behind.
func TestAnAbandonedBackupLeavesNoCopyBehind(t *testing.T) {
	a := newAdminEnv(t)
	dataDir := a.dataDir
	big := bytes.Repeat([]byte("x"), 200<<10)
	in, _ := a.store.GetInboxBySlug(t.Context(), "agent-a")
	in.MaxBodyBytes = 1 << 20
	if err := a.store.UpdateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for range 150 { // about 30 MiB
		if _, _, err := a.store.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: big, Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", a.base+"/v1/admin/backup", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	// The copy exists while the download is under way.
	if left, _ := filepath.Glob(filepath.Join(dataDir, "backup-*")); len(left) != 1 {
		t.Errorf("during the download: %v", left)
	}
	cancel()
	_ = resp.Body.Close()
	waitFor(t, "the abandoned backup's copy to be removed", func() bool {
		left, _ := filepath.Glob(filepath.Join(dataDir, "backup-*"))
		return len(left) == 0
	})
	// And the next backup is not told one is running. The copy is removed a moment
	// before the abandoned request lets go of the backup lock, so a 409 right after
	// is correct and must be brief; anything but a 200 in the end is not.
	var status int
	waitFor(t, "the abandoned backup to let go of the lock", func() bool {
		status, _ = a.admin("GET", "/v1/admin/backup", nil)
		return status != http.StatusConflict
	})
	if status != 200 {
		t.Errorf("a backup after an abandoned one: %d", status)
	}
}

// http.Server.Shutdown waits for requests but does not cancel them. A prune or a
// download started from the admin API ends when shutdown begins instead.
func TestManualRetentionStopsWhenShutdownBegins(t *testing.T) {
	st := &slowToStop{entered: make(chan struct{}, 1)}
	e := newAgentEnv2(t, func(o *Options) {
		st.Store = o.Store
		o.Store = st
		o.RetentionInterval = -1
		o.ShutdownTimeout = 5 * time.Second
	})
	l, err := newListener(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- e.server.Serve(ctx, l) }()
	base := "http://" + l.Addr().String()

	reply := make(chan int, 1)
	go func() {
		status, _ := do2(t, "POST", base+"/v1/admin/retention/run", http.Header{"Authorization": {"Bearer " + testAdminToken}}, "")
		reply <- status
	}()
	<-st.entered
	begin := time.Now()
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v: a manual prune held shutdown until its timeout", err)
		}
		if d := time.Since(begin); d > 3*time.Second {
			t.Errorf("shutdown took %v with a manual prune running", d)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("shutdown never finished")
	}
	<-reply
}
