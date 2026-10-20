// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// AuthFixture is what AuthenticatorSuite needs from an implementation: the
// authenticator itself, and two inboxes it knows, each with a working token.
type AuthFixture struct {
	Authenticator inbox.Authenticator
	// InboxA and InboxB are two different inboxes with different IDs and slugs.
	InboxA, InboxB *inbox.Inbox
	// TokenA reads InboxA and nothing else; TokenB reads InboxB and nothing else.
	TokenA, TokenB string
	// TokenBoth, if non-empty, reads InboxA and InboxB and cannot administer.
	// TokenAll, if non-empty, reads every inbox and cannot administer.
	TokenBoth, TokenAll string
	// Revoke, if set, withdraws a token (one of TokenA, TokenB, TokenBoth, TokenAll) so
	// that it no longer authenticates, as the Store's DeleteToken does for the default
	// implementation. The suite checks that the very next Authenticate refuses it: an
	// Authenticator that remembers who it has seen would not.
	Revoke func(token string)
	// AdminToken, if non-empty, can read every inbox and administer. An
	// implementation without an admin credential leaves it empty.
	AdminToken string
	// UnroutedHost, if non-empty, is a host (as in Request.Host) that serves no org,
	// for an Authenticator that answers a request for such a host with an error that
	// implements inbox.Unrouted, so the server answers 404 instead of 401. The suite
	// checks that every caller of that host, whatever its credential, is refused in
	// the same way, and that a refusal on any other host is not a request for a 404.
	// An implementation with no such host leaves it empty.
	UnroutedHost string
}

// unroutedAnswer reports whether err asks the server for a 404 (inbox.Unrouted),
// the way the server reads it.
func unroutedAnswer(err error) bool {
	var u inbox.Unrouted
	return errors.As(err, &u) && u.UnroutedHost()
}

// AuthenticatorSuite runs the inbox.Authenticator and inbox.Principal
// conformance tests. newFixture must return a fresh fixture on every call.
func AuthenticatorSuite(t *testing.T, newFixture func(t *testing.T) AuthFixture) {
	t.Helper()

	authenticate := func(t *testing.T, f AuthFixture, headers ...string) (inbox.Principal, error) {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/inboxes/x/messages", nil)
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Add(headers[i], headers[i+1])
		}
		p, err := f.Authenticator.Authenticate(r)
		if err == nil && (p == nil || reflect.ValueOf(p).Kind() == reflect.Pointer && reflect.ValueOf(p).IsNil()) {
			t.Fatal("Authenticate returned no error and a nil Principal; it must return one or the other")
		}
		return p, err
	}
	bearer := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

	t.Run("AgentTokenReadsOnlyItsOwnInbox", func(t *testing.T) {
		f := newFixture(t)
		pa, err := authenticate(t, f, bearer(f.TokenA)...)
		if err != nil {
			t.Fatal(err)
		}
		if pa.CanAdmin() {
			t.Error("an agent principal must not be able to administer")
		}
		if !pa.CanReadInbox(f.InboxA) {
			t.Error("a token cannot read its own inbox")
		}
		if pa.CanReadInbox(f.InboxB) {
			t.Error("a token can read another inbox")
		}
		if pa.CanReadInbox(nil) {
			t.Error("a nil inbox must not be readable")
		}
		pb, err := authenticate(t, f, bearer(f.TokenB)...)
		if err != nil || !pb.CanReadInbox(f.InboxB) || pb.CanReadInbox(f.InboxA) {
			t.Errorf("token B: %v, %v", pb, err)
		}
	})

	// A principal limited to some inboxes may say which. It must name exactly the
	// inboxes it reads, and nothing that can administer may claim to be limited.
	t.Run("AScopedPrincipalNamesItsInboxes", func(t *testing.T) {
		f := newFixture(t)
		pa, err := authenticate(t, f, bearer(f.TokenA)...)
		if err != nil {
			t.Fatal(err)
		}
		if sc, ok := pa.(inbox.ScopedPrincipal); ok {
			if ids, err := sc.InboxIDs(t.Context()); err != nil || len(ids) != 1 || ids[0] != f.InboxA.ID {
				t.Errorf("InboxIDs = %v, %v, but the token reads inbox %d", ids, err, f.InboxA.ID)
			}
		}
		if f.TokenBoth != "" {
			p, err := authenticate(t, f, bearer(f.TokenBoth)...)
			if err != nil {
				t.Fatal(err)
			}
			if p.CanAdmin() || !p.CanReadInbox(f.InboxA) || !p.CanReadInbox(f.InboxB) {
				t.Error("a token for both inboxes should read both and not administer")
			}
			if p.CanReadInbox(nil) {
				t.Error("a nil inbox must not be readable")
			}
			if sc, ok := p.(inbox.ScopedPrincipal); ok {
				if ids, err := sc.InboxIDs(t.Context()); err != nil || len(ids) != 2 {
					t.Errorf("InboxIDs = %v, %v, want both inboxes", ids, err)
				}
			}
		}
		for name, tok := range map[string]string{"admin": f.AdminToken, "all-inboxes": f.TokenAll} {
			if tok == "" {
				continue
			}
			p, err := authenticate(t, f, bearer(tok)...)
			if err != nil {
				t.Fatal(err)
			}
			if !p.CanReadInbox(f.InboxA) || !p.CanReadInbox(f.InboxB) {
				t.Errorf("the %s credential should read every inbox", name)
			}
			if p.CanReadInbox(nil) {
				t.Errorf("a nil inbox must not be readable by the %s credential", name)
			}
			if name == "all-inboxes" && p.CanAdmin() {
				t.Error("a token for all inboxes must not be able to administer")
			}
			if _, ok := p.(inbox.ScopedPrincipal); ok {
				t.Errorf("the %s credential must not claim to be limited to some inboxes", name)
			}
		}
	})

	// A hosted implementation will have inboxes that share slugs across tenants,
	// and slugs can change meaning. Authorization must follow the immutable ID.
	t.Run("ReadAccessFollowsTheInboxIDNotItsName", func(t *testing.T) {
		f := newFixture(t)
		pa, err := authenticate(t, f, bearer(f.TokenA)...)
		if err != nil {
			t.Fatal(err)
		}
		impostor := *f.InboxB
		impostor.Slug, impostor.Name = f.InboxA.Slug, f.InboxA.Name
		if pa.CanReadInbox(&impostor) {
			t.Error("another inbox that carries A's slug and name became readable")
		}
		renamed := *f.InboxA
		renamed.Slug, renamed.Name = "some-other-slug", "Some other name"
		if !pa.CanReadInbox(&renamed) {
			t.Error("the same inbox under another slug stopped being readable")
		}
	})

	// Disabling an inbox stops ingest. Agents still drain what is there.
	t.Run("DisablingAnInboxDoesNotRevokeReading", func(t *testing.T) {
		f := newFixture(t)
		pa, err := authenticate(t, f, bearer(f.TokenA)...)
		if err != nil {
			t.Fatal(err)
		}
		off := *f.InboxA
		off.Disabled = true
		if !pa.CanReadInbox(&off) {
			t.Error("a disabled inbox is no longer readable")
		}
	})

	t.Run("AdminReadsEverythingAndAdministers", func(t *testing.T) {
		f := newFixture(t)
		if f.AdminToken == "" {
			t.Skip("this implementation has no admin credential")
		}
		p, err := authenticate(t, f, bearer(f.AdminToken)...)
		if err != nil {
			t.Fatal(err)
		}
		if !p.CanAdmin() || !p.CanReadInbox(f.InboxA) || !p.CanReadInbox(f.InboxB) {
			t.Error("the admin credential should administer and read every inbox")
		}
	})

	t.Run("EveryBadCredentialIsRejectedTheSameWay", func(t *testing.T) {
		f := newFixture(t)
		cut := func(s string) string { return s[:len(s)-1] }
		cases := map[string][]string{
			"no header":                       {},
			"empty header":                    {"Authorization", ""},
			"scheme only":                     {"Authorization", "Bearer"},
			"scheme and a space":              {"Authorization", "Bearer "},
			"another scheme":                  {"Authorization", "Basic " + f.TokenA},
			"no scheme":                       {"Authorization", f.TokenA},
			"an unknown token":                bearer("not-a-real-token-" + strings.Repeat("x", 30)),
			"a token plus a suffix":           bearer(f.TokenA + "x"),
			"a truncated token":               bearer(cut(f.TokenA)),
			"a token with a space":            bearer(f.TokenA + " extra"),
			"a huge token":                    bearer(strings.Repeat("a", 100_000)),
			"two valid Authorization headers": {"Authorization", "Bearer " + f.TokenA, "Authorization", "Bearer " + f.TokenB},
			"a token in a cookie":             {"Cookie", "token=" + f.TokenA},
			"a token in another header":       {"X-Api-Key", f.TokenA},
		}
		if f.AdminToken != "" {
			cases["a truncated admin token"] = bearer(cut(f.AdminToken))
			cases["an admin token plus a suffix"] = bearer(f.AdminToken + "x")
		}
		var first string
		for name, h := range cases {
			p, err := authenticate(t, f, h...)
			if p != nil || !errors.Is(err, inbox.ErrUnauthenticated) {
				t.Errorf("%s: principal %v, error %v; want a nil principal and ErrUnauthenticated", name, p, err)
				continue
			}
			if first == "" {
				first = err.Error()
			} else if err.Error() != first {
				t.Errorf("%s: the error text differs from the others, which would tell a caller why:\n %q\n %q", name, err.Error(), first)
			}
			for _, secret := range []string{f.TokenA, f.TokenB, f.AdminToken} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Errorf("%s: the error contains a token", name)
				}
			}
		}
		// A token in the URL is not a credential either.
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/inboxes/x/messages?access_token="+f.TokenA+"&token="+f.TokenA, nil)
		if p, err := f.Authenticator.Authenticate(r); p != nil || !errors.Is(err, inbox.ErrUnauthenticated) {
			t.Errorf("a token in the query string authenticated: %v, %v", p, err)
		}
	})

	t.Run("AnUnroutedHostIsRefusedTheSameWayForEveryCaller", func(t *testing.T) {
		f := newFixture(t)
		if f.UnroutedHost == "" {
			t.Skip("this implementation has no host that serves no org")
		}
		checkUnroutedHostIsTheSameForEveryCaller(t, f)
	})

	t.Run("ABadCredentialOnAServedHostIsNeverA404", func(t *testing.T) {
		checkServedHostRefusalsAreNot404(t, newFixture(t))
	})

	t.Run("ARevokedTokenIsRefusedAtOnce", func(t *testing.T) {
		f := newFixture(t)
		if f.Revoke == nil {
			t.Skip("this implementation cannot revoke a token through the fixture")
		}
		for range 2 { // the first use must not make it remembered
			if _, err := authenticate(t, f, bearer(f.TokenA)...); err != nil {
				t.Fatal(err)
			}
		}
		f.Revoke(f.TokenA)
		if p, err := authenticate(t, f, bearer(f.TokenA)...); p != nil || !errors.Is(err, inbox.ErrUnauthenticated) {
			t.Errorf("a revoked token: principal %v, error %v; want a nil principal and ErrUnauthenticated", p, err)
		}
		if _, err := authenticate(t, f, bearer(f.TokenB)...); err != nil {
			t.Errorf("revoking one token stopped another: %v", err)
		}
	})

	t.Run("TheSchemeIsCaseInsensitive", func(t *testing.T) {
		f := newFixture(t)
		for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
			if _, err := authenticate(t, f, "Authorization", scheme+" "+f.TokenA); err != nil {
				t.Errorf("scheme %q rejected: %v", scheme, err)
			}
		}
	})
}

// checkUnroutedHostIsTheSameForEveryCaller requires that every caller of the host
// that serves no org, whatever it presents, is refused in one way: no principal,
// ErrUnauthenticated, the same error text, and the same answer to inbox.Unrouted.
// A valid token is refused too, since the host serves nothing to read.
func checkUnroutedHostIsTheSameForEveryCaller(t testing.TB, f AuthFixture) {
	t.Helper()
	bearer := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }
	callers := map[string][]string{
		"no credential":       {},
		"a valid agent token": bearer(f.TokenA),
		"another agent token": bearer(f.TokenB),
		"an unknown token":    bearer("not-a-real-token-" + strings.Repeat("x", 30)),
		"a truncated token":   bearer(f.TokenA[:len(f.TokenA)-1]),
		"another scheme":      {"Authorization", "Basic " + f.TokenA},
		"a token in a cookie": {"Cookie", "token=" + f.TokenA},
		"an empty header":     {"Authorization", ""},
		"two Authorization":   {"Authorization", "Bearer " + f.TokenA, "Authorization", "Bearer " + f.TokenB},
	}
	if f.AdminToken != "" {
		callers["the admin token"] = bearer(f.AdminToken)
	}
	if f.TokenAll != "" {
		callers["a token for every inbox"] = bearer(f.TokenAll)
	}
	var firstName, firstText string
	var firstUnrouted bool
	for name, h := range callers {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/inboxes/x/messages", nil)
		r.Host = f.UnroutedHost
		for i := 0; i+1 < len(h); i += 2 {
			r.Header.Add(h[i], h[i+1])
		}
		p, err := f.Authenticator.Authenticate(r)
		if p != nil || !errors.Is(err, inbox.ErrUnauthenticated) {
			t.Errorf("%s on %s: principal %v, error %v; want a nil principal and ErrUnauthenticated, for a valid token too: the host serves no org", name, f.UnroutedHost, p, err)
			continue
		}
		for _, secret := range []string{f.TokenA, f.TokenB, f.TokenAll, f.AdminToken} {
			if secret != "" && strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error contains a token", name)
			}
		}
		un := unroutedAnswer(err)
		if firstName == "" {
			firstName, firstText, firstUnrouted = name, err.Error(), un
			continue
		}
		if un != firstUnrouted {
			t.Errorf("%s asks for a 404 = %v but %s = %v: the answer for a host that serves no org must not depend on the credential", name, un, firstName, firstUnrouted)
		}
		if err.Error() != firstText {
			t.Errorf("%s: the error text differs from %s, which would tell a caller about its credential:\n %q\n %q", name, firstName, err.Error(), firstText)
		}
	}
}

// checkServedHostRefusalsAreNot404 requires that a bad credential on a host that does
// serve an org is not answered with a request for a 404, which would tell the caller
// that the credential, and not the host, was the problem.
func checkServedHostRefusalsAreNot404(t testing.TB, f AuthFixture) {
	t.Helper()
	cases := map[string][]string{
		"no header":         {},
		"an unknown token":  {"Authorization", "Bearer not-a-real-token-" + strings.Repeat("x", 30)},
		"a truncated token": {"Authorization", "Bearer " + f.TokenA[:len(f.TokenA)-1]},
		"another scheme":    {"Authorization", "Basic " + f.TokenA},
	}
	for name, h := range cases {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/inboxes/x/messages", nil)
		for i := 0; i+1 < len(h); i += 2 {
			r.Header.Add(h[i], h[i+1])
		}
		_, err := f.Authenticator.Authenticate(r)
		if errors.Is(err, inbox.ErrUnauthenticated) && unroutedAnswer(err) {
			t.Errorf("%s: the error asks for a 404 on a host that serves an org. Every bad credential there must get the same 401; only a host that serves none may get a 404", name)
		}
	}
}
