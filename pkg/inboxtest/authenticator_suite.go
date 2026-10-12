// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
		if err == nil && p == nil {
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
			if ids := sc.InboxIDs(); len(ids) != 1 || ids[0] != f.InboxA.ID {
				t.Errorf("InboxIDs = %v, but the token reads inbox %d", ids, f.InboxA.ID)
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
			if sc, ok := p.(inbox.ScopedPrincipal); ok && len(sc.InboxIDs()) != 2 {
				t.Errorf("InboxIDs = %v, want both inboxes", sc.InboxIDs())
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
