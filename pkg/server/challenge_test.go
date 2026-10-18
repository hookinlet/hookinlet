// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// challengingError is an Authenticator error that chooses the 401 challenge.
type challengingError struct{ challenge string }

func (e challengingError) Error() string           { return "unauthenticated" }
func (e challengingError) Unwrap() error           { return inbox.ErrUnauthenticated }
func (e challengingError) WWWAuthenticate() string { return e.challenge }

// refusingAuth refuses every request with err.
type refusingAuth struct{ err error }

func (a refusingAuth) Authenticate(*http.Request) (inbox.Principal, error) { return nil, a.err }

func TestValidChallenge(t *testing.T) {
	good := []string{
		"Bearer",
		`Bearer realm="hookinlet"`,
		`Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`,
		`Bearer realm="x", error="invalid_token"`,
		`Bearer realm="a", Basic realm="b"`,
		"Negotiate",
		"X-Custom.scheme~1 a=b",
		"Bearer  two-spaces",
		strings.Repeat("a", maxChallengeBytes),
	}
	for _, v := range good {
		if !validChallenge(v) {
			t.Errorf("%q refused", v)
		}
	}
	bad := map[string]string{
		"empty":               "",
		"only a space":        " ",
		"leading space":       ` Bearer realm="x"`,
		"no scheme":           `realm="x"`,
		"starts with a quote": `"Bearer" x`,
		"scheme then comma":   `Bearer,realm="x"`,
		"scheme then equals":  `Bearer=x`,
		"scheme then tab":     "Bearer\trealm=x",
		"scheme with a slash": `Bea/rer x`,
		"LF":                  "Bearer realm=\"x\"\nSet-Cookie: a=b",
		"CR":                  "Bearer realm=\"x\"\rSet-Cookie: a=b",
		"CRLF":                "Bearer\r\nX-Injected: 1",
		"trailing LF":         "Bearer realm=\"x\"\n",
		"NUL":                 "Bearer \x00",
		"DEL":                 "Bearer \x7f",
		"tab inside":          "Bearer a=\tb",
		"other control":       "Bearer \x1b[0m",
		"non-ASCII":           "Bearer realm=\"café\"",
		"too long":            strings.Repeat("a", maxChallengeBytes+1),
	}
	for name, v := range bad {
		if validChallenge(v) {
			t.Errorf("%s: %q accepted", name, v)
		}
	}
}

// The challenge is chosen at the one place the server writes a 401, so it holds for
// the REST read API, the admin API, a stream, the MCP endpoint and the 405 that is
// checked for a credential first.
var challengeRequests = []struct{ method, path string }{
	{"GET", "/v1/inboxes/alerts"},
	{"GET", "/v1/inboxes/alerts/messages"},
	{"GET", "/v1/inboxes/alerts/stream"},
	{"POST", "/mcp"},
	{"GET", "/v1/admin/status"},
	{"DELETE", "/v1/inboxes/alerts"},
}

func challengeOf(t *testing.T, s *Server, method, path string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%s %s: status %d, want 401: %s", method, path, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Values("WWW-Authenticate"); len(got) != 1 {
		t.Fatalf("%s %s: %d WWW-Authenticate headers: %q", method, path, len(got), got)
	}
	return rec.Header().Get("WWW-Authenticate")
}

func TestAnAuthenticatorChoosesTheChallenge(t *testing.T) {
	const want = `Bearer resource_metadata="https://hooks.example.com/.well-known/oauth-protected-resource"`
	for name, err := range map[string]error{
		"directly":         challengingError{want},
		"wrapped once":     fmt.Errorf("checking the token: %w", challengingError{want}),
		"wrapped in a set": fmt.Errorf("a: %w", fmt.Errorf("b: %w", challengingError{want})),
	} {
		e := newEnv(t, func(o *Options) { o.Authenticator = refusingAuth{err} })
		for _, r := range challengeRequests {
			if got := challengeOf(t, e.server, r.method, r.path); got != want {
				t.Errorf("%s: %s %s: WWW-Authenticate %q, want %q", name, r.method, r.path, got, want)
			}
		}
	}
}

func TestAChallengeThatIsNotValidFallsBackToTheServersOwn(t *testing.T) {
	for name, v := range map[string]string{
		"empty":           "",
		"multi-line":      "Bearer realm=\"x\"\nSet-Cookie: a=b",
		"CR":              "Bearer\rrealm=x",
		"control":         "Bearer \x01",
		"tab":             "Bearer\trealm=x",
		"not a scheme":    `realm="x"`,
		"bad scheme char": `Bear(er) x`,
		"leading space":   " Bearer",
		"non-ASCII":       "Bearer é",
		"too long":        "Bearer " + strings.Repeat("a", maxChallengeBytes),
	} {
		e := newEnv(t, func(o *Options) { o.Authenticator = refusingAuth{challengingError{v}} })
		for _, r := range challengeRequests {
			if got := challengeOf(t, e.server, r.method, r.path); got != `Bearer realm="hookinlet"` {
				t.Errorf("%s: %s %s: WWW-Authenticate %q", name, r.method, r.path, got)
			}
		}
		if !strings.Contains(e.logs.String(), "ignoring the WWW-Authenticate value") {
			t.Errorf("%s: the invalid value was not logged", name)
		}
		if n := strings.Count(e.logs.String(), "ignoring the WWW-Authenticate value"); n != 1 {
			t.Errorf("%s: logged %d times, want once", name, n)
		}
	}
}

func TestWithoutAChallengerTheChallengeIsTheServersOwn(t *testing.T) {
	for name, err := range map[string]error{
		"bare":    inbox.ErrUnauthenticated,
		"wrapped": fmt.Errorf("no: %w", inbox.ErrUnauthenticated),
	} {
		e := newEnv(t, func(o *Options) { o.Authenticator = refusingAuth{err} })
		for _, r := range challengeRequests {
			if got := challengeOf(t, e.server, r.method, r.path); got != `Bearer realm="hookinlet"` {
				t.Errorf("%s: %s %s: WWW-Authenticate %q", name, r.method, r.path, got)
			}
		}
		if strings.Contains(e.logs.String(), "ignoring the WWW-Authenticate value") {
			t.Errorf("%s: logged a complaint with no Challenger", name)
		}
	}
}

// A Challenger on an error that is not an ErrUnauthenticated does not turn a server
// error into a 401.
func TestAChallengerOnAnotherErrorChangesNothing(t *testing.T) {
	err := fmt.Errorf("store down: %w", otherChallenging{})
	e := newEnv(t, func(o *Options) { o.Authenticator = refusingAuth{err} })
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/v1/inboxes/alerts", nil)
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("status %d, WWW-Authenticate %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

type otherChallenging struct{}

func (otherChallenging) Error() string           { return "boom" }
func (otherChallenging) WWWAuthenticate() string { return "Bearer x" }
