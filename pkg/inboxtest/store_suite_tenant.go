// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// tenantInbox creates an inbox in the tenant ctx is scoped to, with an ingest key
// of its own so the same slug can be used in more than one tenant.
func tenantInbox(t *testing.T, ctx context.Context, s inbox.Store, slug, key string) *inbox.Inbox { //nolint:revive // ctx first would hide that it is the point
	t.Helper()
	in := mkInbox(slug, func(in *inbox.Inbox) { in.IngestKey = key })
	if err := s.CreateInbox(ctx, in); err != nil {
		t.Fatalf("CreateInbox(%s, %s): %v", slug, key, err)
	}
	return in
}

func testTenants(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	ctxA := inbox.WithTenant(ctx, "org-a")
	ctxB := inbox.WithTenant(ctx, "org-b")

	// An inbox made without a scope is in the default tenant, and so is one made
	// in a context scoped to it.
	plain := createInbox(t, s, "plain")
	if plain.Tenant != "" {
		t.Errorf("an inbox made without a scope has tenant %q, want the default (empty)", plain.Tenant)
	}
	if got, err := s.GetInboxBySlug(inbox.WithTenant(ctx, ""), "plain"); err != nil || got.ID != plain.ID {
		t.Errorf("a context scoped to the default tenant does not find it: %v, %v", got, err)
	}

	// Two tenants may each have a slug.
	a := tenantInbox(t, ctxA, s, "shared", "key-a")
	b := tenantInbox(t, ctxB, s, "shared", "key-b")
	if a.Tenant != "org-a" || b.Tenant != "org-b" || a.ID == b.ID {
		t.Fatalf("tenants %q and %q, ids %d and %d", a.Tenant, b.Tenant, a.ID, b.ID)
	}
	for name, c := range map[string]struct {
		ctx  context.Context //nolint:containedctx // a test table
		want *inbox.Inbox
	}{"org-a": {ctxA, a}, "org-b": {ctxB, b}} {
		if got, err := s.GetInboxBySlug(c.ctx, "shared"); err != nil || got.ID != c.want.ID || got.Tenant != name {
			t.Errorf("GetInboxBySlug in %s: %+v, %v; want inbox %d", name, got, err, c.want.ID)
		}
	}
	// A slug names an inbox in one tenant only: without a scope that is the default one.
	_, err := s.GetInboxBySlug(ctx, "shared")
	isErr(t, "a slug that exists only in other tenants, without a scope", err, inbox.ErrNotFound)
	// Within a tenant it is still unique.
	dup := mkInbox("shared", func(in *inbox.Inbox) { in.IngestKey = "key-a2" })
	isErr(t, "a slug used twice in one tenant", s.CreateInbox(ctxA, dup), inbox.ErrConflict)

	// Without a scope an inbox can be made for a tenant by naming it.
	named := mkInbox("named", func(in *inbox.Inbox) { in.Tenant = "org-c" })
	if err := s.CreateInbox(ctx, named); err != nil || named.Tenant != "org-c" {
		t.Errorf("CreateInbox naming a tenant: %q, %v", named.Tenant, err)
	}
	_, err = s.GetInboxBySlug(ctx, "named")
	isErr(t, "an inbox of another tenant found by slug without a scope", err, inbox.ErrNotFound)
	// With one, it can only be for that tenant, and the key has to be valid.
	wrong := mkInbox("wrong", func(in *inbox.Inbox) { in.Tenant = "org-b" })
	isErr(t, "an inbox for another tenant than the scope", s.CreateInbox(ctxA, wrong), inbox.ErrInvalid)
	long := mkInbox("long", func(in *inbox.Inbox) { in.Tenant = strings.Repeat("t", inbox.MaxTenantLen+1) })
	isErr(t, "a tenant key that is too long", s.CreateInbox(ctx, long), inbox.ErrInvalid)

	// Ingest keys and UIDs name an inbox on their own, so they are unique across tenants.
	clash := mkInbox("clash", func(in *inbox.Inbox) { in.IngestKey = "key-a" })
	isErr(t, "an ingest key used in another tenant", s.CreateInbox(ctxB, clash), inbox.ErrConflict)
	clash = mkInbox("clash", func(in *inbox.Inbox) { in.IngestKey = "key-clash"; in.UID = a.UID })
	isErr(t, "a uid used in another tenant", s.CreateInbox(ctxB, clash), inbox.ErrConflict)

	// What tenant A's inbox holds.
	appendN(t, s, a.ID, 3, nil)
	before, err := s.GetInbox(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Tenant B can do nothing with it, by any route.
	notFound := func(what string, err error) {
		t.Helper()
		isErr(t, what+" from another tenant", err, inbox.ErrNotFound)
	}
	_, err = s.GetInbox(ctxB, a.ID)
	notFound("GetInbox", err)
	_, err = s.ResolveIngestKey(ctxB, a.IngestKey)
	notFound("ResolveIngestKey", err)
	_, _, err = s.Append(ctxB, a.ID, nm())
	notFound("Append", err)
	_, _, err = s.Append(ctxB, a.ID, nm(func(m *inbox.NewMessage) { m.Verified = true; m.DeliveryID = "d1" }))
	notFound("Append of a verified delivery", err)
	_, err = s.Query(ctxB, a.ID, inbox.Query{})
	notFound("Query", err)
	_, err = s.Get(ctxB, a.ID, 1)
	notFound("Get", err)
	_, err = s.LatestSeq(ctxB, a.ID)
	notFound("LatestSeq", err)
	_, err = s.Stats(ctxB, a.ID)
	notFound("Stats", err)
	if c, ok := s.(inbox.UnverifiedCounter); ok {
		_, err = c.CountUnverified(ctxB, a.ID, 10)
		notFound("CountUnverified", err)
	}
	edit := *before
	edit.Name = "taken over"
	notFound("UpdateInbox", s.UpdateInbox(ctxB, &edit))
	notFound("DeleteInbox", s.DeleteInbox(ctxB, a.ID))
	isErr(t, "a token for another tenant's inbox",
		s.CreateToken(ctxB, &inbox.Token{Name: "t", Hash: tokenHash("cross-tenant"), InboxIDs: []inbox.ID{a.ID}}), inbox.ErrNotFound)
	if _, err := s.GetTokenByHash(ctx, tokenHash("cross-tenant")); err == nil {
		t.Error("a token was stored although its inbox belongs to another tenant")
	}

	// Listing shows a tenant its own inboxes, and the operator's view shows all.
	slugsOf := func(c context.Context) []string { //nolint:revive // a helper
		t.Helper()
		var out []string
		var after inbox.ID
		for {
			page, err := s.ListInboxes(c, inbox.ListParams{AfterID: after, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range page {
				out = append(out, in.Tenant+"/"+in.Slug)
				after = in.ID
			}
			if len(page) < 2 {
				return out
			}
		}
	}
	if got := slugsOf(ctxB); len(got) != 1 || got[0] != "org-b/shared" {
		t.Errorf("tenant B lists %v, want only its own", got)
	}
	if got := slugsOf(ctx); len(got) != 4 { // plain, a, b, named
		t.Errorf("the operator lists %v, want 4 inboxes", got)
	}

	// None of that touched it, and the owner and the operator still reach it.
	after, err := s.GetInbox(ctxA, a.ID)
	if err != nil || after.Version != before.Version || after.LastSeq != 3 || after.Name != before.Name {
		t.Errorf("tenant A's inbox after the attempts: %+v, %v", after, err)
	}
	if _, created, err := s.Append(ctx, a.ID, nm()); err != nil || !created { // a delivery is not for a tenant
		t.Errorf("Append without a scope: %v, created %v", err, created)
	}
	if got, err := s.ResolveIngestKey(ctx, a.IngestKey); err != nil || got.ID != a.ID {
		t.Errorf("ResolveIngestKey without a scope: %v, %v", got, err)
	}
	if p := query(t, s, a.ID, inbox.Query{}); len(p.Messages) != 4 {
		t.Errorf("tenant A's inbox holds %d messages, want 4", len(p.Messages))
	}

	// The tenant never changes, whatever an update carries.
	moved := *after
	moved.Tenant = "org-b"
	if err := s.UpdateInbox(ctxA, &moved); err != nil || moved.Tenant != "org-a" {
		t.Errorf("an update that names another tenant: kept %q, %v", moved.Tenant, err)
	}

	// The owner can delete it, and the slug is then free in that tenant.
	if err := s.DeleteInbox(ctxA, a.ID); err != nil {
		t.Fatal(err)
	}
	if again := tenantInbox(t, ctxA, s, "shared", "key-a-again"); again.ID == a.ID {
		t.Error("the new inbox has the deleted one's id")
	}
	if got, err := s.GetInboxBySlug(ctxB, "shared"); err != nil || got.ID != b.ID {
		t.Errorf("tenant B's inbox after tenant A's was deleted: %v, %v", got, err)
	}
}

// testTokensAreNotScopedByTenant pins what the Store contract says of tokens today:
// they are the operator's. A Token has no tenant, and GetTokenByHash, ListTokens and
// DeleteToken act on every token whatever tenant the context is scoped to; the only
// check a scope adds is CreateToken's, that the token's inboxes are the tenant's
// (the Tenants case checks that). GetTokenByHash must ignore the scope in any case,
// since the server calls it to authenticate a request before it knows the tenant.
//
// Whether tokens should instead belong to a tenant is an open design question. This
// case checks only the current contract, so that a Store does not quietly differ
// from it; if the contract changes, this case changes with it.
func testTokensAreNotScopedByTenant(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	ctxA := inbox.WithTenant(ctx, "org-a")
	ctxB := inbox.WithTenant(ctx, "org-b")
	a := tenantInbox(t, ctxA, s, "tokens", "tokens-key-a")
	b := tenantInbox(t, ctxB, s, "tokens", "tokens-key-b")

	tokA := mkToken("of-a", scoped(a.ID))
	if err := s.CreateToken(ctxA, tokA); err != nil {
		t.Fatalf("CreateToken in tenant A for its own inbox: %v", err)
	}
	tokB := mkToken("of-b", scoped(b.ID))
	if err := s.CreateToken(ctxB, tokB); err != nil {
		t.Fatalf("CreateToken in tenant B for its own inbox: %v", err)
	}

	// Found by its hash from any context, as authentication needs.
	for name, c := range map[string]context.Context{"unscoped": ctx, "tenant A": ctxA, "tenant B": ctxB} {
		got, err := s.GetTokenByHash(c, tokenHash("of-a"))
		if err != nil || got.ID != tokA.ID || len(got.InboxIDs) != 1 || got.InboxIDs[0] != a.ID {
			t.Errorf("GetTokenByHash of tenant A's token, %s: %+v, %v; want it, as without a scope", name, got, err)
		}
	}

	// Listed the same from any context, with or without an inbox to filter by.
	ids := func(c context.Context, p inbox.TokenListParams) []inbox.TokenID { //nolint:revive // a helper
		t.Helper()
		tks, err := s.ListTokens(c, p)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		out := []inbox.TokenID{}
		for _, tk := range tks {
			out = append(out, tk.ID)
		}
		return out
	}
	for name, p := range map[string]inbox.TokenListParams{
		"every token":          {},
		"tokens of A's inbox":  {InboxID: a.ID},
		"tokens of B's inbox":  {InboxID: b.ID},
		"after the first, one": {AfterID: tokA.ID, Limit: 1},
	} {
		want := ids(ctx, p)
		for scope, c := range map[string]context.Context{"tenant A": ctxA, "tenant B": ctxB} {
			if got := ids(c, p); !reflect.DeepEqual(got, want) {
				t.Errorf("ListTokens (%s) in %s = %v; want %v, as without a scope", name, scope, got, want)
			}
		}
	}
	if got := ids(ctxB, inbox.TokenListParams{}); !reflect.DeepEqual(got, []inbox.TokenID{tokA.ID, tokB.ID}) {
		t.Errorf("ListTokens in tenant B = %v; want both tenants' tokens %v", got, []inbox.TokenID{tokA.ID, tokB.ID})
	}

	// Deleted by its ID from any context.
	if err := s.DeleteToken(ctxB, tokA.ID); err != nil {
		t.Errorf("DeleteToken of tenant A's token in tenant B: %v; want it deleted, as without a scope", err)
	}
	_, err := s.GetTokenByHash(ctx, tokenHash("of-a"))
	isErr(t, "a token deleted from another tenant's context", err, inbox.ErrNotFound)
	isErr(t, "a second delete, from its own tenant", s.DeleteToken(ctxA, tokA.ID), inbox.ErrNotFound)
	if got, err := s.GetTokenByHash(ctxA, tokenHash("of-b")); err != nil || got.ID != tokB.ID {
		t.Errorf("tenant B's token after tenant A's was deleted: %+v, %v", got, err)
	}
}
