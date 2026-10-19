// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// unroutedError is an Authenticator error that asks for a 404 when answers is true.
type unroutedError struct{ answers bool }

func (e unroutedError) Error() string      { return "unauthenticated" }
func (e unroutedError) Unwrap() error      { return inbox.ErrUnauthenticated }
func (e unroutedError) UnroutedHost() bool { return e.answers }

// hostAuth refuses every request. For the host nowhere.example it asks for a 404,
// as an embedder's Authenticator does for a host that serves no one; for any other
// it refuses as an ordinary 401.
type hostAuth struct{}

const unservedHost = "nowhere.example"

func (hostAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	if r.Host == unservedHost {
		return nil, fmt.Errorf("host %s: %w", r.Host, unroutedError{true})
	}
	return nil, inbox.ErrUnauthenticated
}

// authenticatedOperations are the operations of the document that need a credential,
// each with a concrete request line.
type authenticatedOperation struct{ id, method, path string }

func authenticatedOperations(t *testing.T) []authenticatedOperation {
	t.Helper()
	sp := loadSpec(t)
	fill := strings.NewReplacer("{slug}", "alerts", "{seq}", "1", "{id}", "1")
	var out []authenticatedOperation
	for _, o := range sp.operations() {
		if sec, _ := o.obj["security"].([]any); len(sec) == 0 {
			continue
		}
		out = append(out, authenticatedOperation{o.id, o.method, fill.Replace(o.path)})
	}
	if len(out) < 20 {
		t.Fatalf("found only %d authenticated operations in the document", len(out))
	}
	return out
}

// answer is everything a client can compare in a response, except what is meant to
// differ between requests.
func answer(rec *httptest.ResponseRecorder) string {
	h := rec.Header().Clone()
	h.Del("X-Request-Id")
	h.Del("Date")
	var names []string
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", rec.Code)
	for _, k := range names {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(h[k], ","))
	}
	b.WriteString(rec.Body.String())
	return b.String()
}

func serveHost(s *Server, method, path, host string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(""))
	if host != "" {
		req.Host = host
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// For a host that serves no org, every authenticated operation answers the server's
// ordinary 404, the same bytes for every credential, with no challenge.
func TestAnUnroutedHostAnswers404ForEveryAuthenticatedOperation(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Authenticator = hostAuth{} })
	// What the server answers for an address it has no route for is the shape to match.
	wantBody := serveHost(e.server, "GET", "/no/such/path", "").Body.String()
	if !strings.Contains(wantBody, `"not_found"`) {
		t.Fatalf("unexpected 404 body %q", wantBody)
	}
	credentials := [][]string{
		nil,
		{"Authorization", "Bearer garbage"},
		{"Authorization", "Bearer hki_" + strings.Repeat("a", 40)},
		{"Authorization", "Basic dXNlcjpwYXNz"},
		{"Authorization", "Bearer a", "Authorization", "Bearer b"},
		{"Cookie", "token=x"},
	}
	for _, op := range authenticatedOperations(t) {
		methods := []string{op.method}
		if op.method == "GET" {
			methods = append(methods, "HEAD")
		}
		methods = append(methods, "TRACE") // a method the path does not take: the 405 checks the credential first
		for _, m := range methods {
			var first string
			for i, cred := range credentials {
				rec := serveHost(e.server, m, op.path, unservedHost, cred...)
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s (%s): status %d, want 404", m, op.path, op.id, rec.Code)
					continue
				}
				if got := rec.Header().Values("WWW-Authenticate"); len(got) != 0 {
					t.Errorf("%s %s: WWW-Authenticate %q on a 404", m, op.path, got)
				}
				if m != "HEAD" && rec.Body.String() != wantBody {
					t.Errorf("%s %s: body %q, want %q", m, op.path, rec.Body.String(), wantBody)
				}
				if i == 0 {
					first = answer(rec)
				} else if got := answer(rec); got != first {
					t.Errorf("%s %s: the answer depends on the credential %v:\n%s\n--- versus ---\n%s", m, op.path, cred, got, first)
				}
			}
		}
	}
}

// A host that is served is refused as before: a 401 with the challenge.
func TestARoutedHostIsStillRefusedWith401(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Authenticator = hostAuth{} })
	for _, op := range authenticatedOperations(t) {
		rec := serveHost(e.server, op.method, op.path, "hooks.example.com")
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: status %d, WWW-Authenticate %q; want 401 with a challenge", op.method, op.path, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
}

// An error that has the method but says false is a 401; so is a bare Unrouted error
// found deeper in a chain, and one that is not about authentication is not looked at.
func TestOnlyAnUnroutedErrorThatSaysSoIsA404(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
	}{
		"says true":                  {unroutedError{true}, 404},
		"says true, wrapped":         {fmt.Errorf("a: %w", fmt.Errorf("b: %w", unroutedError{true})), 404},
		"says false":                 {unroutedError{false}, 401},
		"a plain ErrNotFound":        {inbox.ErrNotFound, 500},
		"ErrNotFound with a message": {fmt.Errorf("no such host: %w", inbox.ErrNotFound), 500},
		"unrouted but not unauthenticated": {
			fmt.Errorf("store down: %w", unroutedOnly{}), 500,
		},
	} {
		e := newEnv(t, func(o *Options) { o.Authenticator = refusingAuth{tc.err} })
		for _, r := range challengeRequests {
			req := httptest.NewRequestWithContext(t.Context(), r.method, r.path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			e.server.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Errorf("%s: %s %s: status %d, want %d", name, r.method, r.path, rec.Code, tc.status)
			}
			if tc.status != 401 && rec.Header().Get("WWW-Authenticate") != "" {
				t.Errorf("%s: %s %s: WWW-Authenticate on a %d", name, r.method, r.path, rec.Code)
			}
		}
	}
}

// unroutedOnly asks for a 404 but does not wrap ErrUnauthenticated.
type unroutedOnly struct{}

func (unroutedOnly) Error() string      { return "boom" }
func (unroutedOnly) UnroutedHost() bool { return true }

// A stream already open ends when its host stops being served, like any other
// refusal, instead of carrying on.
func TestAStreamEndsWhenItsHostBecomesUnrouted(t *testing.T) {
	// The stream's periodic re-check treats any ErrUnauthenticated as "no longer
	// allowed", so an Unrouted error needs no case of its own: this guards that.
	ok, err := (&Server{opts: Options{Authenticator: refusingAuth{unroutedError{true}}}}).
		stillAllowed(httptest.NewRequestWithContext(t.Context(), "GET", "/v1/inboxes/alerts/stream", nil), 1)(t.Context())
	if ok || err != nil {
		t.Errorf("stillAllowed = %v, %v; want false, nil", ok, err)
	}
}

// unroutedHostScenario: every authenticated operation documents the 404, and a test sees it: the scenario
// calls each one against a host that serves no org, so TestOpenAPIMatchesTheServer
// holds the document to it.
func unroutedHostScenario(t *testing.T, c *contract) {
	c.t = t
	a := newAdminEnv(t, func(o *Options) { o.Authenticator = refusingAuth{unroutedError{true}} })
	cl := c.on(a.base)
	for _, op := range authenticatedOperations(t) {
		cl.call(op.id, op.method, op.path, nil, "")
		if st, _, _ := cl.call(op.id, op.method, op.path, nil, "some-token"); st != http.StatusNotFound {
			t.Errorf("%s: status %d with a token, want 404", op.id, st)
		}
	}
}
