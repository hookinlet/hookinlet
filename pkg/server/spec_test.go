// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/docs"
	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/specvalidate"
)

// holdTo replaces the document the server enforces with the embedded one after
// change has edited it. The server has to follow the document, so a document
// that asks for more must get more.
func holdTo(t *testing.T, s *Server, change func(doc map[string]any)) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(docs.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := specvalidate.Load(b)
	if err != nil {
		t.Fatal(err)
	}
	s.spec = spec
}

func at(v any, keys ...string) map[string]any {
	for _, k := range keys {
		v = v.(map[string]any)[k]
	}
	return v.(map[string]any)
}

// param returns the schema of a named query parameter of an operation.
func param(t *testing.T, doc map[string]any, path, method, name string) map[string]any {
	t.Helper()
	for _, p := range at(doc, "paths", path, method)["parameters"].([]any) {
		pm := p.(map[string]any)
		if ref, ok := pm["$ref"].(string); ok {
			pm = at(doc, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...)
		}
		if pm["name"] == name {
			sc := pm["schema"].(map[string]any)
			if ref, ok := sc["$ref"].(string); ok {
				return at(doc, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...)
			}
			return sc
		}
	}
	t.Fatalf("no parameter %s on %s %s", name, method, path)
	return nil
}

func TestTheServerRefusesWhatTheDocumentForbids(t *testing.T) {
	e := newAgentEnv(t)
	path := "/v1/inboxes/agent-a/messages"
	for _, q := range []string{"?limit=6", "?limit=5"} {
		if rec := e.get(path+q, e.tokenA); rec.Code != http.StatusOK {
			t.Fatalf("%s before the document changes: %d %s", q, rec.Code, rec.Body)
		}
	}
	holdTo(t, e.server, func(doc map[string]any) {
		param(t, doc, "/v1/inboxes/{slug}/messages", "get", "limit")["maximum"] = 5
	})
	if rec := e.get(path+"?limit=5", e.tokenA); rec.Code != http.StatusOK {
		t.Errorf("limit=5 within the new maximum: %d %s", rec.Code, rec.Body)
	}
	rec := e.get(path+"?limit=6", e.tokenA)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `parameter \"limit\": must be at most 5`) {
		t.Errorf("limit=6 past the new maximum: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"invalid_parameter"`) {
		t.Errorf("the usual error shape was not used: %s", rec.Body)
	}
}

func TestTheDocumentDecidesWhichParametersExist(t *testing.T) {
	e := newAgentEnv(t)
	path := "/v1/inboxes/agent-a/messages"
	holdTo(t, e.server, func(doc map[string]any) {
		ps := at(doc, "paths", "/v1/inboxes/{slug}/messages", "get")["parameters"].([]any)
		keep := ps[:0]
		for _, p := range ps {
			pm := p.(map[string]any)
			if ref, ok := pm["$ref"].(string); ok {
				pm = at(doc, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...)
			}
			if pm["name"] != "verified" {
				keep = append(keep, p)
			}
		}
		at(doc, "paths", "/v1/inboxes/{slug}/messages", "get")["parameters"] = keep
	})
	rec := e.get(path+"?verified=true", e.tokenA)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `parameter \"verified\": unknown parameter`) {
		t.Errorf("a parameter the document no longer lists: %d %s", rec.Code, rec.Body)
	}
}

func TestTheGuardsRunAfterAuthentication(t *testing.T) {
	e := newAgentEnv(t)
	// A bad query from a caller who may not read gets what any such caller gets,
	// so the document's parameters are not an oracle for who is allowed.
	if rec := e.get("/v1/inboxes/agent-a/messages?bogus=1", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous with a bad query: %d", rec.Code)
	}
	if rec := e.get("/v1/inboxes/agent-a/messages?bogus=1", e.tokenB); rec.Code != http.StatusNotFound {
		t.Errorf("another inbox's token with a bad query: %d", rec.Code)
	}
	if rec := e.get("/v1/inboxes/agent-a/stream?bogus=1", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous stream with a bad query: %d", rec.Code)
	}
}

func TestAdminBodiesAreHeldToTheDocument(t *testing.T) {
	a := newAdminEnv(t)
	body := map[string]any{"slug": "tighter", "verifier": "none", "name": "abcd"}
	if st, data := a.admin(http.MethodPost, "/admin/inboxes", body); st != http.StatusCreated {
		t.Fatalf("before the document changes: %d %s", st, data)
	}
	holdTo(t, a.server, func(doc map[string]any) {
		at(doc, "components", "schemas", "CreateInbox", "properties", "name")["maxLength"] = 3
	})
	body["slug"] = "tighter-2"
	st, data := a.admin(http.MethodPost, "/admin/inboxes", body)
	if st != http.StatusBadRequest || !strings.Contains(string(data), "name: must be at most 3 character(s)") {
		t.Errorf("a name past the new maximum: %d %s", st, data)
	}
	for _, tc := range []struct{ body, want string }{
		{`{"slug":"bad-types","verifier":"none","retention_days":"7"}`, "retention_days: must be an integer"},
		{`{"slug":"bad-types","verifier":"none","retention_days":-1}`, "retention_days: must be at least 0"},
		{`{"slug":"bad-types","verifier":"none","retention_days":99999999999999999999}`, "retention_days: must be at most 36500"},
		{`{"slug":"bad-types","verifier":"none","nope":1}`, "nope: is not a known field"},
		{`{"slug":"X","verifier":"none"}`, "slug: is not in the required form"},
		{`{}`, "slug: is required"},
		{`[]`, "must be an object"},
	} {
		st, data := a.admin(http.MethodPost, "/admin/inboxes", tc.body)
		if st != http.StatusBadRequest || !strings.Contains(string(data), tc.want) {
			t.Errorf("%s: %d %s, want 400 containing %q", tc.body, st, data, tc.want)
		}
	}
}

func TestTheAdminQueryGuardRunsAfterAuthentication(t *testing.T) {
	a := newAdminEnv(t)
	st, _, _ := a.call(http.MethodGet, "/admin/inboxes?bogus=1", nil, "")
	if st != http.StatusUnauthorized {
		t.Errorf("anonymous with a bad query: %d", st)
	}
	if st, _, _ := a.call(http.MethodGet, "/admin/inboxes?bogus=1", nil, a.tokenA); st != http.StatusForbidden {
		t.Errorf("an agent token with a bad query: %d", st)
	}
	if st, _, _ := a.call(http.MethodPost, "/admin/inboxes?bogus=1", `{"slug":"abc","verifier":"none"}`, ""); st != http.StatusUnauthorized {
		t.Errorf("anonymous POST with a bad query: %d", st)
	}
}

func TestEveryAdminOperationRefusesAnUnknownQueryParameter(t *testing.T) {
	a := newAdminEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/inboxes?x=1"},
		{http.MethodGet, "/admin/inboxes/agent-a?x=1"},
		{http.MethodDelete, "/admin/inboxes/agent-a?x=1"},
		{http.MethodGet, "/admin/tokens?x=1"},
		{http.MethodPost, "/admin/tokens?x=1"},
		{http.MethodDelete, "/admin/tokens/1?x=1"},
		{http.MethodPost, "/admin/inboxes/agent-a/rotate-ingest-key?x=1"},
		{http.MethodPost, "/admin/retention/run?x=1"},
		{http.MethodGet, "/admin/backup?x=1"},
	} {
		// The response is also checked against the document, which has to list 400.
		st, data := a.admin(tc.method, tc.path, nil)
		if st != http.StatusBadRequest || !strings.Contains(string(data), `parameter \"x\": unknown parameter`) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, st, data)
		}
	}
	// And nothing was deleted by the refused DELETE.
	a.mustAdmin(http.MethodGet, "/admin/inboxes/agent-a", nil, http.StatusOK)
}

func TestAnAdminListAcceptsAnyCursorAnInt64Can(t *testing.T) {
	a := newAdminEnv(t)
	a.mustAdmin(http.MethodGet, "/admin/inboxes?after=9223372036854775807", nil, http.StatusOK)
}

func TestARefusedBodyDoesNotRepeatALongFieldName(t *testing.T) {
	a := newAdminEnv(t)
	body := `{"slug":"clipped","verifier":"none","` + strings.Repeat("k", 50000) + `":1}`
	st, data := a.admin(http.MethodPost, "/admin/inboxes", body)
	if st != http.StatusBadRequest {
		t.Fatalf("status %d", st)
	}
	if len(data) > 1000 {
		t.Errorf("the answer is %d bytes", len(data))
	}
}

func TestAnAdminRouteMissingFromTheDocumentIsRefusedNotServedUnchecked(t *testing.T) {
	a := newAdminEnv(t)
	holdTo(t, a.server, func(doc map[string]any) {
		delete(at(doc, "paths", "/admin/inboxes"), "post")
	})
	st, data := a.admin(http.MethodPost, "/admin/inboxes", map[string]any{"slug": "unchecked", "verifier": "none"})
	if st != http.StatusInternalServerError {
		t.Errorf("a route the document lacks: %d %s", st, data)
	}
}

func TestVerifierConfigNumbersSurviveTheResponse(t *testing.T) {
	v := admin.View{VerifierConfig: json.RawMessage(`{"big":9007199254740993,"x":1.50}`)}
	out, err := json.Marshal(view(v, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"big":9007199254740993`) || !strings.Contains(string(out), `"x":1.50`) {
		t.Errorf("config numbers changed: %s", out)
	}
}

// A request the generated code cannot even read (a parameter of the wrong type) is
// still answered as the credential deserves: nothing about parameter checking is
// learned by someone who may not use the operation.
func TestAMalformedParameterNeverBeatsTheCredentialCheck(t *testing.T) {
	a := newAdminEnv(t)
	a.tokenB = a.tokenFor(a.newInbox("agent-b", "trusted", ""))
	for _, tc := range []struct {
		name, method, path, tok string
		want                    int
	}{
		{"anonymous, bad after", http.MethodGet, "/v1/inboxes/agent-a/messages?after=abc", "", http.StatusUnauthorized},
		{"anonymous, bad seq", http.MethodGet, "/v1/inboxes/agent-a/messages/abc", "", http.StatusUnauthorized},
		{"anonymous, bad stream cursor", http.MethodGet, "/v1/inboxes/agent-a/stream?after=x", "", http.StatusUnauthorized},
		{"another inbox's token, bad after", http.MethodGet, "/v1/inboxes/agent-a/messages?after=abc", a.tokenB, http.StatusNotFound},
		{"another inbox's token, bad seq", http.MethodGet, "/v1/inboxes/agent-a/messages/abc", a.tokenB, http.StatusNotFound},
		{"an agent on the admin API, bad limit", http.MethodGet, "/admin/inboxes?limit=abc", a.tokenA, http.StatusForbidden},
		{"anonymous on the admin API, bad limit", http.MethodGet, "/admin/inboxes?limit=abc", "", http.StatusUnauthorized},
	} {
		st, _, data := a.call(tc.method, tc.path, nil, tc.tok)
		if st != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, st, data, tc.want)
		}
	}
}

// A delivery is not refused for its URL, and a probe is not refused for a
// cache-busting query string.
func TestIngestAndHealthChecksTakeAnyQueryString(t *testing.T) {
	a := newAdminEnv(t)
	for _, path := range []string{"/in/agent-a?a=1;b=2", "/in/agent-a?a=%zz", "/in/agent-a/x?a=1;b=2"} {
		if st, _, data := a.call(http.MethodPost, path, "{}", ""); st != http.StatusOK {
			t.Errorf("POST %s: %d %s", path, st, data)
		}
	}
	for _, path := range []string{"/healthz?cb=123", "/readyz?cb=123"} {
		if st, _, data := a.call(http.MethodGet, path, nil, ""); st != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, st, data)
		}
	}
}
