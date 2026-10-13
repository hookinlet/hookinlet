// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package admin_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// interfering changes the inbox behind the caller's back before each of the
// first N updates, as an edit from another process would: the caller's copy is
// then stale and the version check refuses it.
type interfering struct {
	inbox.Store
	remaining atomic.Int64
	updates   atomic.Int64
}

func (s *interfering) UpdateInbox(ctx context.Context, in *inbox.Inbox) error {
	s.updates.Add(1)
	if s.remaining.Add(-1) >= 0 {
		other, err := s.GetInbox(ctx, in.ID)
		if err != nil {
			return err
		}
		other.Name = other.Name + "+"
		if err := s.Store.UpdateInbox(ctx, other); err != nil {
			return err
		}
	}
	return s.Store.UpdateInbox(ctx, in)
}

func newService(t *testing.T, interferences int64) (*admin.Service, *interfering, *sqlite.Store) {
	t.Helper()
	real, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = real.Close() })
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	st := &interfering{Store: real}
	st.remaining.Store(interferences)
	svc := &admin.Service{Store: st, Box: box}
	if _, err := svc.Create(t.Context(), admin.CreateParams{Slug: "retry-inbox", Verifier: "token"}); err != nil {
		t.Fatal(err)
	}
	st.updates.Store(0)
	return svc, st, real
}

// An edit from another process lands between the read and the write: the change
// is applied again on top of it, and neither is lost.
func TestEditsAreRetriedWhenAnotherProcessGotThereFirst(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := map[string]func(svc *admin.Service) error{
		"update": func(svc *admin.Service) error {
			_, err := svc.Update(t.Context(), "retry-inbox", admin.Patch{Name: str("mine")})
			return err
		},
		"set secret": func(svc *admin.Service) error {
			_, err := svc.SetSecret(t.Context(), "retry-inbox", "a-secret")
			return err
		},
	}
	for name, do := range cases {
		svc, st, real := newService(t, 2)
		before, _ := real.GetInboxBySlug(t.Context(), "retry-inbox")
		if err := do(svc); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := st.updates.Load(); got != 3 {
			t.Errorf("%s: %d update attempts, want 3 (two stale, one good)", name, got)
		}
		after, _ := real.GetInboxBySlug(t.Context(), "retry-inbox")
		if after.Name == before.Name && name != "update" && len(after.VerifierSecret) == len(before.VerifierSecret) {
			t.Errorf("%s: nothing changed", name)
		}
		// The other process's edits are still there.
		if name != "update" && after.Name != "retry-inbox++" {
			t.Errorf("%s: the interfering edits were lost: name %q", name, after.Name)
		}
		if name == "update" && after.Name != "mine" {
			t.Errorf("update: name %q", after.Name)
		}
	}
}

func TestEditsGiveUpWhenTheInboxKeepsChanging(t *testing.T) {
	str := func(s string) *string { return &s }
	svc, st, _ := newService(t, 1_000)
	_, err := svc.Update(t.Context(), "retry-inbox", admin.Patch{Name: str("never")})
	if !errors.Is(err, inbox.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}
	if got := st.updates.Load(); got < 2 || got > 20 {
		t.Errorf("%d attempts before giving up", got)
	}
}

func TestIssuedTokensAreNeverReusedAndCanBeListedAndRevoked(t *testing.T) {
	svc, _, real := newService(t, 0)
	ctx := t.Context()
	seen := map[string]bool{}
	var ids []int64
	// The inbox came with its default token.
	if first, _, err := svc.ListTokens(ctx, 0, 0, ""); err != nil || len(first) != 1 || first[0].Name != admin.DefaultTokenName {
		t.Fatalf("tokens at the start: %+v, %v", first, err)
	}
	for i := range 5 {
		c, err := svc.CreateToken(ctx, admin.TokenParams{Name: fmt.Sprintf("t%d", i), Inboxes: []string{"retry-inbox"}})
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.Token] {
			t.Fatal("a token was issued twice")
		}
		seen[c.Token] = true
		ids = append(ids, c.Record.ID)
		stored, err := real.GetTokenByHash(ctx, token.Hash(c.Token))
		if err != nil || int64(stored.ID) != c.Record.ID {
			t.Errorf("the issued token is not the stored one: %v", err)
		}
		if !reflect.DeepEqual(c.Record.Inboxes, []string{"retry-inbox"}) || c.Record.AllInboxes {
			t.Errorf("record = %+v", c.Record)
		}
	}
	all, err := svc.CreateToken(ctx, admin.TokenParams{Name: "everything", AllInboxes: true})
	if err != nil || !all.Record.AllInboxes || len(all.Record.Inboxes) != 0 {
		t.Fatalf("all-inboxes token: %+v, %v", all, err)
	}

	// Paging, and the filter by inbox.
	page, next, err := svc.ListTokens(ctx, 0, 4, "")
	if err != nil || len(page) != 4 || next == 0 {
		t.Fatalf("first page: %d tokens, next %d, %v", len(page), next, err)
	}
	rest, next, err := svc.ListTokens(ctx, next, 4, "")
	if err != nil || len(rest) != 3 || next != 0 {
		t.Fatalf("second page: %d tokens, next %d, %v", len(rest), next, err)
	}
	if forInbox, _, err := svc.ListTokens(ctx, 0, 0, "retry-inbox"); err != nil || len(forInbox) != 7 {
		t.Errorf("tokens for the inbox: %d, %v; want its 6 and the all-inboxes one", len(forInbox), err)
	}
	if _, _, err := svc.ListTokens(ctx, 0, 0, "no-such-inbox"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("an unknown inbox: %v", err)
	}

	// Revoking one leaves the others.
	if err := svc.RevokeToken(ctx, inbox.TokenID(ids[0])); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeToken(ctx, inbox.TokenID(ids[0])); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("revoking twice: %v", err)
	}
	if left, _, _ := svc.ListTokens(ctx, 0, 0, ""); len(left) != 6 {
		t.Errorf("%d tokens left, want 6", len(left))
	}
}

func TestTokenRequestsAreChecked(t *testing.T) {
	svc, _, _ := newService(t, 0)
	many := make([]string, inbox.MaxTokenInboxes+1)
	for i := range many {
		many[i] = "retry-inbox"
	}
	for name, tc := range map[string]struct {
		p    admin.TokenParams
		want error
	}{
		"neither inboxes nor all": {admin.TokenParams{Name: "x"}, inbox.ErrInvalid},
		"both inboxes and all":    {admin.TokenParams{Name: "x", Inboxes: []string{"retry-inbox"}, AllInboxes: true}, inbox.ErrInvalid},
		"no name":                 {admin.TokenParams{Inboxes: []string{"retry-inbox"}}, inbox.ErrInvalid},
		"a name that is too long": {admin.TokenParams{Name: strings.Repeat("n", 101), AllInboxes: true}, inbox.ErrInvalid},
		"too many inboxes":        {admin.TokenParams{Name: "x", Inboxes: many}, inbox.ErrInvalid},
		"an inbox twice":          {admin.TokenParams{Name: "x", Inboxes: []string{"retry-inbox", "retry-inbox"}}, inbox.ErrInvalid},
		"an unknown inbox":        {admin.TokenParams{Name: "x", Inboxes: []string{"nope"}}, inbox.ErrNotFound},
	} {
		if _, err := svc.CreateToken(t.Context(), tc.p); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestCheckSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		p       admin.CreateParams
		wantErr string
	}{
		{"minimal", admin.CreateParams{Slug: "abc"}, ""},
		{"bad slug", admin.CreateParams{Slug: "A"}, "slug"},
		{"bad on_verify_fail", admin.CreateParams{Slug: "abc", OnVerifyFail: "maybe"}, "on_verify_fail"},
		{"body too big", admin.CreateParams{Slug: "abc", MaxBodyBytes: 1 << 40}, "max_body_bytes"},
		{"bad pointer", admin.CreateParams{Slug: "abc", EventTypePointer: "type"}, "event_type_pointer"},
		{"known verifier, bad config", admin.CreateParams{Slug: "abc", Verifier: "stripe", VerifierConfig: []byte(`{"tolerance_seconds":"x"}`)}, "tolerance"},
		{"known verifier, good config", admin.CreateParams{Slug: "abc", Verifier: "stripe", VerifierConfig: []byte(`{"tolerance_seconds":300}`)}, ""},
		{"unknown verifier is the server's call", admin.CreateParams{Slug: "abc", Verifier: "custom-one"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := admin.CheckSettings(tc.p)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// The service itself refuses an inbox with no verifier, so the command line's
// direct mode and any embedder that uses it cannot get one by default.
func TestCreateRequiresAVerifier(t *testing.T) {
	svc, _, _ := newService(t, 0)
	_, err := svc.Create(t.Context(), admin.CreateParams{Slug: "undecided"})
	if !errors.Is(err, inbox.ErrInvalid) || !strings.Contains(err.Error(), "verifier is required") {
		t.Fatalf("Create with no verifier: %v", err)
	}
	c, err := svc.Create(t.Context(), admin.CreateParams{Slug: "decided", Verifier: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Inbox.IngestKey) < 20 || c.Inbox.IngestKey == "decided" {
		t.Errorf("the ingest key is not random: %q", c.Inbox.IngestKey)
	}
}

// Sending a verifier the inbox already has changes nothing, secret included: a
// client that re-sends its settings must not lose the secret.
func TestPatchWithTheSameVerifierKeepsTheSecret(t *testing.T) {
	svc, _, _ := newService(t, 0)
	if _, err := svc.Create(t.Context(), admin.CreateParams{Slug: "keeps-secret", Verifier: "github", VerifierSecret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	same := "github"
	v, err := svc.Update(t.Context(), "keeps-secret", admin.Patch{Verifier: &same})
	if err != nil || !v.HasVerifierSecret || len(v.Warnings) != 0 {
		t.Fatalf("same verifier: %+v, %v", v, err)
	}
	other := "stripe"
	v, err = svc.Update(t.Context(), "keeps-secret", admin.Patch{Verifier: &other})
	if err != nil || v.HasVerifierSecret || len(v.Warnings) == 0 {
		t.Fatalf("another verifier: %+v, %v", v, err)
	}
}

// vanishing deletes an inbox right after the tokens are read, as a delete from
// another request would between the two reads a token listing makes.
type vanishing struct {
	inbox.Store
	victim inbox.ID
	done   atomic.Bool
}

func (s *vanishing) ListTokens(ctx context.Context, p inbox.TokenListParams) ([]*inbox.Token, error) {
	tokens, err := s.Store.ListTokens(ctx, p)
	if err == nil && s.done.CompareAndSwap(false, true) {
		err = s.DeleteInbox(ctx, s.victim)
	}
	return tokens, err
}

// An inbox deleted while tokens are being listed is not a reason to fail the
// listing: the other tokens are still wanted, and the deleted inbox's tokens are
// being revoked.
func TestListingTokensSurvivesAnInboxDeletedMeanwhile(t *testing.T) {
	_, _, real := newService(t, 0)
	ctx := t.Context()
	st := &vanishing{Store: real}
	svc := &admin.Service{Store: st}
	for _, slug := range []string{"keeps", "goes"} {
		if _, err := svc.Create(ctx, admin.CreateParams{Slug: slug, Verifier: "none"}); err != nil {
			t.Fatal(err)
		}
	}
	goes, err := real.GetInboxBySlug(ctx, "goes")
	if err != nil {
		t.Fatal(err)
	}
	st.victim = goes.ID
	both, err := svc.CreateToken(ctx, admin.TokenParams{Name: "both", Inboxes: []string{"keeps", "goes"}})
	if err != nil {
		t.Fatal(err)
	}

	views, _, err := svc.ListTokens(ctx, 0, 0, "")
	if err != nil {
		t.Fatalf("listing while an inbox was being deleted: %v", err)
	}
	var seen bool
	for _, v := range views {
		if v.ID == both.Record.ID {
			seen = true
			if !reflect.DeepEqual(v.Inboxes, []string{"keeps"}) {
				t.Errorf("the token's inboxes = %v, want only the one that is left", v.Inboxes)
			}
		}
		if v.Name == admin.DefaultTokenName && len(v.Inboxes) == 0 {
			t.Errorf("a token with no inbox left was listed: %+v", v)
		}
	}
	if !seen {
		t.Errorf("the token for both inboxes is missing from %+v", views)
	}
}
