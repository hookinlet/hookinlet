// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

const testUnroutedHost = "nowhere.example"

// hostError is a refusal that may ask for a 404.
type hostError struct {
	unrouted bool
	detail   string
}

func (e hostError) Error() string      { return "unauthenticated" + e.detail }
func (e hostError) Unwrap() error      { return inbox.ErrUnauthenticated }
func (e hostError) UnroutedHost() bool { return e.unrouted }

type fakePrincipal struct{ admin bool }

func (fakePrincipal) CanReadInbox(in *inbox.Inbox) bool { return in != nil }
func (p fakePrincipal) CanAdmin() bool                  { return p.admin }

// hostAuthenticator knows three tokens. refuse decides what it answers for a request
// it refuses, given the request and whether the host is the unrouted one.
type hostAuthenticator struct {
	tokens map[string]bool // token -> admin
	refuse func(r *http.Request, unroutedHost bool) error
}

func (a hostAuthenticator) Authenticate(r *http.Request) (inbox.Principal, error) {
	unrouted := r.Host == testUnroutedHost
	tok := ""
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		tok = h[7:]
	}
	if admin, ok := a.tokens[tok]; ok && !unrouted && len(r.Header.Values("Authorization")) == 1 {
		return fakePrincipal{admin}, nil
	}
	return nil, a.refuse(r, unrouted)
}

const (
	testTokenA, testTokenB, testAdmin = "tok-a-1234567890", "tok-b-1234567890", "tok-admin-1234567890"
)

func hostFixture(refuse func(r *http.Request, unroutedHost bool) error) AuthFixture {
	return AuthFixture{
		Authenticator: hostAuthenticator{map[string]bool{testTokenA: false, testTokenB: false, testAdmin: true}, refuse},
		InboxA:        &inbox.Inbox{ID: 1, Slug: "a"}, InboxB: &inbox.Inbox{ID: 2, Slug: "b"},
		TokenA: testTokenA, TokenB: testTokenB, AdminToken: testAdmin,
		UnroutedHost: testUnroutedHost,
	}
}

func TestTheAuthenticatorChecksCatchWhatTheyAreFor(t *testing.T) {
	for _, c := range []struct {
		name   string
		refuse func(r *http.Request, unroutedHost bool) error
		check  func(testing.TB, AuthFixture)
		want   string
	}{
		{"a good one, on the unrouted host", func(_ *http.Request, u bool) error { return hostError{unrouted: u} }, checkUnroutedHostIsTheSameForEveryCaller, ""},
		{"a good one, on a served host", func(_ *http.Request, u bool) error { return hostError{unrouted: u} }, checkServedHostRefusalsAreNot404, ""},
		{"a plain 401 everywhere", func(*http.Request, bool) error { return hostError{} }, checkUnroutedHostIsTheSameForEveryCaller, ""},
		{
			"a 404 only for a credential that looks valid",
			func(r *http.Request, u bool) error {
				return hostError{unrouted: u && r.Header.Get("Authorization") == "Bearer "+testTokenA}
			},
			checkUnroutedHostIsTheSameForEveryCaller, "must not depend on the credential",
		},
		{
			"error text that says which credential",
			func(r *http.Request, u bool) error {
				return hostError{unrouted: u, detail: fmt.Sprintf(": %d headers", len(r.Header))}
			},
			checkUnroutedHostIsTheSameForEveryCaller, "error text differs",
		},
		{
			"a valid token still reads on the unrouted host",
			nil, // set below: the authenticator itself is replaced
			checkUnroutedHostIsTheSameForEveryCaller, "want a nil principal",
		},
		{
			"a 404 for a bad token on a served host",
			func(_ *http.Request, _ bool) error { return hostError{unrouted: true} },
			checkServedHostRefusalsAreNot404, "serves an org",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := hostFixture(c.refuse)
			if c.refuse == nil {
				f.Authenticator = readsAnywhere{}
			}
			expectFailure(t, c.want, func(rec testing.TB) { c.check(rec, f) })
		})
	}
}

// readsAnywhere authenticates a valid token whatever the host, which a host that
// serves no org must not do.
type readsAnywhere struct{}

func (readsAnywhere) Authenticate(r *http.Request) (inbox.Principal, error) {
	if r.Header.Get("Authorization") == "Bearer "+testTokenA {
		return fakePrincipal{}, nil
	}
	return nil, hostError{unrouted: true}
}
