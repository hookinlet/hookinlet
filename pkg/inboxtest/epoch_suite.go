// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// EpochRenewerSuite runs the inbox.EpochRenewer conformance tests.
//
// newStore must return a new, empty Store on every call, driven by the given
// clock, exactly as for StoreSuite. The Store must implement inbox.EpochRenewer.
// The suite closes each Store itself.
func EpochRenewerSuite(t *testing.T, newStore func(t *testing.T, now func() time.Time) inbox.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(testing.TB, inbox.Store, inbox.EpochRenewer)
	}{
		{"EveryInboxGetsADifferentEpoch", checkRenewChangesEveryEpoch},
		{"EveryTenantIsRenewedWhateverTheScope", checkRenewReachesEveryTenant},
		{"MessagesTokensAndSettingsAreUntouched", checkRenewChangesNothingElse},
		{"CallingItTwiceIsSafe", checkRenewTwice},
		{"AnEmptyStoreIsFine", checkRenewEmpty},
		{"ACancelledContextChangesNothing", checkRenewCancelled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t, NewClock(baseTime).Now)
			t.Cleanup(func() { _ = s.Close() })
			r, ok := s.(inbox.EpochRenewer)
			if !ok {
				t.Fatalf("%T does not implement inbox.EpochRenewer", s)
			}
			c.fn(t, s, r)
		})
	}
}

// epochs reads the epoch of each inbox, by ID.
func epochs(t testing.TB, s inbox.Store, inboxes ...*inbox.Inbox) map[inbox.ID]string {
	t.Helper()
	out := map[inbox.ID]string{}
	for _, in := range inboxes {
		got, err := s.GetInbox(t.Context(), in.ID)
		if err != nil {
			t.Errorf("GetInbox(%s): %v", in.Slug, err)
			continue
		}
		out[in.ID] = got.Epoch
	}
	return out
}

// makeInboxes creates a few inboxes of different kinds: one with messages, one
// disabled, and one that never received anything.
func makeInboxes(t testing.TB, s inbox.Store) []*inbox.Inbox {
	t.Helper()
	var out []*inbox.Inbox
	for i, mod := range []func(*inbox.Inbox){
		func(*inbox.Inbox) {},
		func(in *inbox.Inbox) { in.Disabled = true },
		func(in *inbox.Inbox) { in.RetentionMaxCount = 5 },
	} {
		in := mkInbox(fmt.Sprintf("renew-%d", i), mod)
		if err := s.CreateInbox(t.Context(), in); err != nil {
			t.Errorf("CreateInbox: %v", err)
			continue
		}
		if i == 0 {
			for range 3 {
				if _, _, err := s.Append(t.Context(), in.ID, nm()); err != nil {
					t.Errorf("Append: %v", err)
				}
			}
		}
		out = append(out, in)
	}
	return out
}

// A cursor carries the epoch it came from, and the server refuses one that is not
// the inbox's current epoch (409 epoch_changed), so what has to hold is that the
// epoch the store reports differs from every epoch an old cursor can carry, by
// every way of reading an inbox.
func checkRenewChangesEveryEpoch(t testing.TB, s inbox.Store, r inbox.EpochRenewer) {
	t.Helper()
	ctx := t.Context()
	inboxes := makeInboxes(t, s)
	before := epochs(t, s, inboxes...)
	if err := r.RenewEpochs(ctx); err != nil {
		t.Errorf("RenewEpochs: %v", err)
		return
	}
	after := map[string]bool{}
	for _, in := range inboxes {
		got, err := s.GetInbox(ctx, in.ID)
		if err != nil {
			t.Errorf("GetInbox(%s): %v", in.Slug, err)
			continue
		}
		if got.Epoch == "" || got.Epoch == before[in.ID] {
			t.Errorf("%s: epoch %q after RenewEpochs, was %q; a cursor from before would still be accepted", in.Slug, got.Epoch, before[in.ID])
		}
		if after[got.Epoch] {
			t.Errorf("%s: epoch %q is the same as another inbox's", in.Slug, got.Epoch)
		}
		after[got.Epoch] = true
		// Every way of reading the inbox says the same.
		if bySlug, err := s.GetInboxBySlug(ctx, in.Slug); err != nil || bySlug.Epoch != got.Epoch {
			t.Errorf("%s: GetInboxBySlug says epoch %v (%v), GetInbox says %q", in.Slug, bySlug, err, got.Epoch)
		}
		if byKey, err := s.ResolveIngestKey(ctx, in.IngestKey); err != nil || byKey.Epoch != got.Epoch {
			t.Errorf("%s: ResolveIngestKey says epoch %v (%v), GetInbox says %q", in.Slug, byKey, err, got.Epoch)
		}
	}
}

// RenewEpochs is for the whole Store: the scope of the context does not narrow it.
// A Store that does not keep inboxes under tenants has nothing to check.
func checkRenewReachesEveryTenant(t testing.TB, s inbox.Store, r inbox.EpochRenewer) {
	t.Helper()
	ctx := t.Context()
	a := mkInbox("renew-tenant-a")
	if err := s.CreateInbox(inbox.WithTenant(ctx, "renew-org-a"), a); err != nil || a.Tenant != "renew-org-a" {
		t.Logf("the Store does not keep inboxes under tenants (%v); nothing to check", err)
		return
	}
	plain := mkInbox("renew-tenant-plain")
	if err := s.CreateInbox(ctx, plain); err != nil {
		t.Errorf("CreateInbox: %v", err)
		return
	}
	before := epochs(t, s, a, plain)
	if err := r.RenewEpochs(inbox.WithTenant(ctx, "renew-org-a")); err != nil {
		t.Errorf("RenewEpochs in a context scoped to a tenant: %v", err)
		return
	}
	for name, in := range map[string]*inbox.Inbox{"the tenant's": a, "another tenant's": plain} {
		if got, err := s.GetInbox(ctx, in.ID); err != nil || got.Epoch == before[in.ID] {
			t.Errorf("%s inbox has epoch %v after RenewEpochs (%v), was %q: it renews every tenant whatever the scope", name, got, err, before[in.ID])
		}
	}
}

// Renewing an epoch is all it does.
func checkRenewChangesNothingElse(t testing.TB, s inbox.Store, r inbox.EpochRenewer) {
	t.Helper()
	ctx := t.Context()
	in := mkInbox("renew-keeps", func(in *inbox.Inbox) {
		in.Verifier = "hmac"
		in.VerifierConfig = []byte(`{"header":"X-Sig"}`)
		in.VerifierSecret = []byte("sealed-bytes")
		in.OnVerifyFail = inbox.OnVerifyFailStore
		in.RetentionDays, in.RetentionMaxCount, in.MaxBodyBytes = 7, 1000, 4096
		in.EventTypePointer = "/type"
	})
	if err := s.CreateInbox(ctx, in); err != nil {
		t.Errorf("CreateInbox: %v", err)
		return
	}
	for i := range 4 {
		if _, _, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) {
			m.DeliveryID = fmt.Sprintf("d%d", i)
			m.Verified = i%2 == 0
		})); err != nil {
			t.Errorf("Append: %v", err)
			return
		}
	}
	scoped := &inbox.Token{Name: "scoped", Hash: tokenHash("renew-scoped"), InboxIDs: []inbox.ID{in.ID}}
	everything := &inbox.Token{Name: "everything", Hash: tokenHash("renew-everything"), AllInboxes: true}
	for _, tok := range []*inbox.Token{scoped, everything} {
		if err := s.CreateToken(ctx, tok); err != nil {
			t.Errorf("CreateToken: %v", err)
			return
		}
	}
	readAll := func() (msgs []*inbox.Message) {
		for seq := int64(1); seq <= 4; seq++ {
			m, err := s.Get(ctx, in.ID, seq)
			if err != nil {
				t.Errorf("Get(%d): %v", seq, err)
				continue
			}
			msgs = append(msgs, m)
		}
		return msgs
	}
	wantInbox, err := s.GetInbox(ctx, in.ID)
	if err != nil {
		t.Errorf("GetInbox: %v", err)
		return
	}
	wantMsgs := readAll()

	if err := r.RenewEpochs(ctx); err != nil {
		t.Errorf("RenewEpochs: %v", err)
		return
	}

	got, err := s.GetInbox(ctx, in.ID)
	if err != nil {
		t.Errorf("GetInbox: %v", err)
		return
	}
	// The epoch changes, and a Store may count that as an update (Version, UpdatedAt).
	if got.Epoch == wantInbox.Epoch {
		t.Errorf("the epoch did not change")
	}
	got.Epoch, got.Version, got.UpdatedAt = wantInbox.Epoch, wantInbox.Version, wantInbox.UpdatedAt
	if !reflect.DeepEqual(got, wantInbox) {
		t.Errorf("RenewEpochs changed more than the epoch:\n got  %+v\n want %+v", got, wantInbox)
	}
	if gotMsgs := readAll(); !reflect.DeepEqual(gotMsgs, wantMsgs) {
		t.Errorf("RenewEpochs changed the messages:\n got  %+v\n want %+v", gotMsgs, wantMsgs)
	}
	for _, want := range []*inbox.Token{scoped, everything} {
		tok, err := s.GetTokenByHash(ctx, want.Hash)
		if err != nil {
			t.Errorf("token %q after RenewEpochs: %v", want.Name, err)
			continue
		}
		if tok.ID != want.ID || tok.Name != want.Name || tok.AllInboxes != want.AllInboxes || !reflect.DeepEqual(tok.InboxIDs, want.InboxIDs) {
			t.Errorf("token %q changed:\n got  %+v\n want %+v", want.Name, tok, want)
		}
	}
	// Sequence numbers carry on, and a delivery stored before is still a duplicate.
	if m, created, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) { m.DeliveryID = "d0" })); err != nil || created || m.Seq != 1 {
		t.Errorf("a repeated delivery after RenewEpochs: seq %v, created %v, %v; want the original, not created", m, created, err)
	}
	if m, _, err := s.Append(ctx, in.ID, nm()); err != nil || m.Seq != 5 {
		t.Errorf("the next message after RenewEpochs: %v, %v; want seq 5", m, err)
	}
}

func checkRenewTwice(t testing.TB, s inbox.Store, r inbox.EpochRenewer) {
	t.Helper()
	ctx := t.Context()
	inboxes := makeInboxes(t, s)
	e0 := epochs(t, s, inboxes...)
	if err := r.RenewEpochs(ctx); err != nil {
		t.Errorf("RenewEpochs: %v", err)
		return
	}
	e1 := epochs(t, s, inboxes...)
	if err := r.RenewEpochs(ctx); err != nil {
		t.Errorf("RenewEpochs a second time: %v", err)
		return
	}
	e2 := epochs(t, s, inboxes...)
	for _, in := range inboxes {
		if e2[in.ID] == "" || e2[in.ID] == e1[in.ID] || e2[in.ID] == e0[in.ID] {
			t.Errorf("%s: epochs %q, %q and %q; each call must give a new one", in.Slug, e0[in.ID], e1[in.ID], e2[in.ID])
		}
	}
	if st, err := s.Stats(ctx, inboxes[0].ID); err != nil || st.MessageCount != 3 || st.LatestSeq != 3 {
		t.Errorf("stats after two renewals: %+v, %v; want the three messages", st, err)
	}
}

func checkRenewEmpty(t testing.TB, _ inbox.Store, r inbox.EpochRenewer) {
	t.Helper()
	if err := r.RenewEpochs(t.Context()); err != nil {
		t.Errorf("RenewEpochs on a Store with no inboxes: %v", err)
	}
}

func checkRenewCancelled(t testing.TB, s inbox.Store, r inbox.EpochRenewer) {
	t.Helper()
	inboxes := makeInboxes(t, s)
	before := epochs(t, s, inboxes...)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.RenewEpochs(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("RenewEpochs on a cancelled context: %v; want an error wrapping context.Canceled", err)
	}
	after := epochs(t, s, inboxes...)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("a cancelled RenewEpochs changed epochs: %v, then %v", before, after)
	}
}
