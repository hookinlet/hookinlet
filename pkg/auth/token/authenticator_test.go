// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package token_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

const adminToken = "admin-token-for-the-test-suite"

type fixture struct {
	auth   *token.Authenticator
	store  *sqlite.Store
	tokenA string
	tokenB string
	// tokenBoth reads A and B; tokenAll reads every inbox; idA is tokenA's ID.
	tokenBoth, tokenAll string
	idA                 inbox.TokenID
	a, b                *inbox.Inbox
}

func newFixture(t *testing.T, admin string) *fixture {
	t.Helper()
	store, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &fixture{store: store, auth: token.NewAuthenticator(store, admin)}
	issue := func(name string, all bool, ids ...inbox.ID) (string, inbox.TokenID) {
		tok, hash, err := token.Generate()
		if err != nil {
			t.Fatal(err)
		}
		rec := &inbox.Token{Name: name, Hash: hash, AllInboxes: all, InboxIDs: ids}
		if err := store.CreateToken(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
		return tok, rec.ID
	}
	mk := func(slug string) *inbox.Inbox {
		in := &inbox.Inbox{Slug: slug, IngestKey: slug, Name: slug, Verifier: "none"}
		if err := store.CreateInbox(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	f.a, f.b = mk("inbox-a"), mk("inbox-b")
	f.tokenA, f.idA = issue("a", false, f.a.ID)
	f.tokenB, _ = issue("b", false, f.b.ID)
	f.tokenBoth, _ = issue("both", false, f.a.ID, f.b.ID)
	f.tokenAll, _ = issue("all", true)
	return f
}

// revoke withdraws a token the way an admin does.
func (f *fixture) revoke(tok string) {
	rec, err := f.store.GetTokenByHash(context.Background(), token.Hash(tok))
	if err != nil {
		panic(err)
	}
	if err := f.store.DeleteToken(context.Background(), rec.ID); err != nil {
		panic(err)
	}
}

func TestConformance(t *testing.T) {
	inboxtest.AuthenticatorSuite(t, func(t *testing.T) inboxtest.AuthFixture {
		f := newFixture(t, adminToken)
		return inboxtest.AuthFixture{
			Authenticator: f.auth, InboxA: f.a, InboxB: f.b, TokenA: f.tokenA, TokenB: f.tokenB, TokenBoth: f.tokenBoth, TokenAll: f.tokenAll, AdminToken: adminToken, Revoke: f.revoke,
		}
	})
	t.Run("without an admin token", func(t *testing.T) {
		inboxtest.AuthenticatorSuite(t, func(t *testing.T) inboxtest.AuthFixture {
			f := newFixture(t, "")
			return inboxtest.AuthFixture{Authenticator: f.auth, InboxA: f.a, InboxB: f.b, TokenA: f.tokenA, TokenB: f.tokenB, TokenBoth: f.tokenBoth, TokenAll: f.tokenAll, Revoke: f.revoke}
		})
	})
}

// hostRouted wraps an Authenticator the way an embedder that serves orgs by host does: for
// a host it serves not at all, whatever the credential, it refuses with an error that asks
// the server for a 404 (inbox.Unrouted); for any other it defers to the wrapped one.
type hostRouted struct {
	inbox.Authenticator
	unservedHost string
}

type unroutedRefusal struct{}

func (unroutedRefusal) Error() string      { return "unauthenticated" }
func (unroutedRefusal) Unwrap() error      { return inbox.ErrUnauthenticated }
func (unroutedRefusal) UnroutedHost() bool { return true }

func (h hostRouted) Authenticate(r *http.Request) (inbox.Principal, error) {
	if r.Host == h.unservedHost {
		return nil, unroutedRefusal{}
	}
	return h.Authenticator.Authenticate(r)
}

// An Authenticator that answers 404 for a host that serves no org still passes the whole
// suite: bad credentials elsewhere are refused alike, and every caller of the unserved
// host gets the same answer.
func TestConformanceOfA404CapableAuthenticator(t *testing.T) {
	inboxtest.AuthenticatorSuite(t, func(t *testing.T) inboxtest.AuthFixture {
		f := newFixture(t, adminToken)
		return inboxtest.AuthFixture{
			Authenticator: hostRouted{f.auth, "nowhere.example"}, InboxA: f.a, InboxB: f.b, TokenA: f.tokenA, TokenB: f.tokenB,
			TokenBoth: f.tokenBoth, TokenAll: f.tokenAll, AdminToken: adminToken, Revoke: f.revoke, UnroutedHost: "nowhere.example",
		}
	})
}

func req(t *testing.T, headers ...string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/inboxes/x", nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Add(headers[i], headers[i+1])
	}
	return r
}

func TestAdminToken(t *testing.T) {
	f := newFixture(t, adminToken)
	p, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanAdmin() || !p.CanReadInbox(f.a) || !p.CanReadInbox(f.b) {
		t.Errorf("the admin should read everything and administer: %#v", p)
	}
}

func TestAgentTokenReadsOnlyItsOwnInbox(t *testing.T) {
	f := newFixture(t, adminToken)
	pa, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA))
	if err != nil {
		t.Fatal(err)
	}
	if pa.CanAdmin() {
		t.Error("an agent must not be an admin")
	}
	if !pa.CanReadInbox(f.a) || pa.CanReadInbox(f.b) {
		t.Errorf("agent A: reads A=%v, reads B=%v", pa.CanReadInbox(f.a), pa.CanReadInbox(f.b))
	}
	if pa.CanReadInbox(nil) {
		t.Error("a nil inbox must not be readable")
	}
	pb, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenB))
	if err != nil || !pb.CanReadInbox(f.b) || pb.CanReadInbox(f.a) {
		t.Errorf("agent B: %v, %v", pb, err)
	}
	if got, err := pa.(token.Agent).InboxIDs(t.Context()); err != nil || len(got) != 1 || got[0] != f.a.ID {
		t.Errorf("InboxIDs = %v, %v, want [%d]", got, err, f.a.ID)
	}
	pall, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenAll))
	if err != nil || pall.CanAdmin() || !pall.CanReadInbox(f.a) || !pall.CanReadInbox(f.b) {
		t.Errorf("an all-inboxes token should read everything and not administer: %v, %v", pall, err)
	}
}

func TestSchemeIsCaseInsensitive(t *testing.T) {
	f := newFixture(t, adminToken)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "bEaReR"} {
		if _, err := f.auth.Authenticate(req(t, "Authorization", scheme+" "+f.tokenA)); err != nil {
			t.Errorf("scheme %q rejected: %v", scheme, err)
		}
	}
}

func TestEveryBadCredentialLooksTheSame(t *testing.T) {
	f := newFixture(t, adminToken)
	other, _, _ := token.Generate()
	cases := map[string][]string{
		"no header":                 {},
		"empty header":              {"Authorization", ""},
		"wrong scheme":              {"Authorization", "Basic " + f.tokenA},
		"no scheme":                 {"Authorization", f.tokenA},
		"scheme only":               {"Authorization", "Bearer"},
		"scheme and space only":     {"Authorization", "Bearer "},
		"unknown agent token":       {"Authorization", "Bearer " + other},
		"admin token, wrong case":   {"Authorization", "Bearer " + strings.ToUpper(adminToken)},
		"admin token plus a suffix": {"Authorization", "Bearer " + adminToken + "x"},
		"admin token prefix":        {"Authorization", "Bearer " + adminToken[:len(adminToken)-1]},
		"agent token plus a suffix": {"Authorization", "Bearer " + f.tokenA + "x"},
		"agent token truncated":     {"Authorization", "Bearer " + f.tokenA[:len(f.tokenA)-1]},
		"token with a space":        {"Authorization", "Bearer " + f.tokenA + " extra"},
		"token with a tab":          {"Authorization", "Bearer\t" + f.tokenA},
		"two spaces":                {"Authorization", "Bearer  " + f.tokenA},
		"huge token":                {"Authorization", "Bearer " + strings.Repeat("a", 100_000)},
		"token with a prefix only":  {"Authorization", "Bearer " + token.Prefix},
		"two headers, both valid":   {"Authorization", "Bearer " + f.tokenA, "Authorization", "Bearer " + f.tokenB},
		"token in the query":        {},
	}
	var messages []string
	for name, h := range cases {
		r := req(t, h...)
		if name == "token in the query" {
			r.URL.RawQuery = "token=" + f.tokenA + "&access_token=" + f.tokenA
		}
		p, err := f.auth.Authenticate(r)
		if p != nil || !errors.Is(err, inbox.ErrUnauthenticated) {
			t.Errorf("%s: principal %v, error %v; want ErrUnauthenticated", name, p, err)
			continue
		}
		messages = append(messages, err.Error())
	}
	for _, m := range messages[1:] {
		if m != messages[0] {
			t.Errorf("failure messages differ: %q vs %q", m, messages[0])
		}
	}
	if strings.Contains(messages[0], f.tokenA) || strings.Contains(messages[0], adminToken) {
		t.Error("the failure message contains a token")
	}
}

func TestNoAdminTokenConfigured(t *testing.T) {
	f := newFixture(t, "")
	for _, tok := range []string{"", "anything", adminToken} {
		if p, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+tok)); err == nil {
			t.Errorf("token %q authenticated as %v with no admin token configured", tok, p)
		}
	}
	if _, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
		t.Errorf("agent tokens must still work: %v", err)
	}
}

func TestRevokedTokenStopsWorkingAndASecondOneKeepsWorking(t *testing.T) {
	f := newFixture(t, adminToken)
	if _, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeleteToken(t.Context(), f.idA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); !errors.Is(err, inbox.ErrUnauthenticated) {
		t.Errorf("the revoked token still works: %v", err)
	}
	if p, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenBoth)); err != nil || !p.CanReadInbox(f.a) {
		t.Errorf("another token for the inbox stopped working: %v, %v", p, err)
	}
}

func TestDeletedInboxTokenStopsWorking(t *testing.T) {
	f := newFixture(t, adminToken)
	if err := f.store.DeleteInbox(t.Context(), f.a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); !errors.Is(err, inbox.ErrUnauthenticated) {
		t.Errorf("a deleted inbox's token still works: %v", err)
	}
}

func TestStoreFailureIsNotReportedAsBadCredentials(t *testing.T) {
	f := newFixture(t, adminToken)
	_ = f.store.Close()
	_, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA))
	if err == nil || errors.Is(err, inbox.ErrUnauthenticated) {
		t.Errorf("a failing store should surface as a server error, got %v", err)
	}
	// The admin token needs no lookup, so it still works.
	if _, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+adminToken)); err != nil {
		t.Errorf("admin: %v", err)
	}
}

func TestAnAgentTokenForADisabledInboxStillAuthenticates(t *testing.T) {
	// Disabling an inbox stops ingest; agents can still drain what is there.
	f := newFixture(t, adminToken)
	off := *f.a
	off.Disabled = true
	if err := f.store.UpdateInbox(t.Context(), &off); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
		t.Errorf("a disabled inbox's agent token was rejected: %v", err)
	}
}

// lookupCounter counts the agent-token lookups that reach the Store.
type lookupCounter struct {
	inbox.Store
	n int
}

func (l *lookupCounter) GetTokenByHash(ctx context.Context, h []byte) (*inbox.Token, error) {
	l.n++
	return l.Store.GetTokenByHash(ctx, h)
}

// Anything that cannot be a token we issued is turned away without touching the
// database, so junk credentials cost an attacker nothing to send and us nothing
// to answer.
func TestObviouslyWrongCredentialsNeverReachTheStore(t *testing.T) {
	f := newFixture(t, adminToken)
	counter := &lookupCounter{Store: f.store}
	auth := token.NewAuthenticator(counter, adminToken)

	for _, tok := range []string{"x", "admin-token-for-the-test-suite-but-wrong", strings.Repeat("z", 500), "Bearer", "hki", "HKI_" + f.tokenA[4:]} {
		if _, err := auth.Authenticate(req(t, "Authorization", "Bearer "+tok)); !errors.Is(err, inbox.ErrUnauthenticated) {
			t.Errorf("token %.20q: %v", tok, err)
		}
	}
	if counter.n != 0 {
		t.Errorf("%d database lookups for credentials that cannot be tokens", counter.n)
	}
	// A real-looking token is checked, once.
	_, _ = auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA))
	if counter.n != 1 {
		t.Errorf("%d lookups for one token, want 1", counter.n)
	}
	// The admin token never needs a lookup.
	_, _ = auth.Authenticate(req(t, "Authorization", "Bearer "+adminToken))
	if counter.n != 1 {
		t.Errorf("the admin token caused a lookup")
	}
}
