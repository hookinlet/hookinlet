// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// StoreOptions adds checks to StoreSuiteWith that need something only the Store's
// own test can do. The zero value is StoreSuite.
type StoreOptions struct {
	// MakeFull makes the Store unable to store another message, as a full disk or
	// a size limit would, and returns a function that undoes it (it may be nil if
	// the Store cannot be given room again). After MakeFull returns, an Append of a
	// message with a body of 1 MiB must fail for lack of room. s is a Store the
	// suite made with newStore, holding an inbox with two messages.
	//
	// With it, the suite checks that such an Append returns an error wrapping
	// inbox.ErrStorageFull (which the server answers with 503, so the sender
	// retries), stores no part of the message, and uses up no sequence number: once
	// there is room again the next message gets the next number, with none missing.
	// When MakeFull is nil that case is skipped.
	MakeFull func(t *testing.T, s inbox.Store) (undo func())

	// Peer returns another Store value on the same backend as s: the Store as a
	// second server of the same deployment sees it. With it, the concurrent
	// Prune case also calls Prune on the peers, which is what several servers
	// that each run the retention sweep do. When Peer is nil, that case calls Prune
	// from several goroutines on s alone. The suite closes the peers itself.
	Peer func(t *testing.T, s inbox.Store) inbox.Store

	// PrepareTenant makes s ready to hold inboxes for the tenant, for a Store that
	// has to be told of a tenant first (an embedder that registers each customer
	// before it stores anything for it). The suite calls it once for each tenant it
	// uses ("org-a", "org-b" and "org-c") on each Store it makes, before the first
	// call that names that tenant, and not for the default tenant (the empty one),
	// which every Store has. A Store that needs no registration leaves it nil, and the
	// suite checks that such a Store passes as before. It is not called for a tenant
	// key that the Store must refuse (one that is too long): a case about a tenant that
	// does not exist or is not valid must be able to reach the Store without it.
	PrepareTenant func(t *testing.T, s inbox.Store, tenant string)
}

// StoreSuiteWith is StoreSuite with a StoreOptions.
func StoreSuiteWith(t *testing.T, newStore func(t *testing.T, now func() time.Time) inbox.Store, opts StoreOptions) {
	t.Helper()
	runStoreSuite(t, newStore, opts)
}

// checkStorageFull is the case behind StoreOptions.MakeFull. It reports through t
// without stopping the test, so that a test of the check itself can read what it
// said.
func checkStorageFull(t testing.TB, s inbox.Store, makeFull func() (undo func())) {
	t.Helper()
	ctx := t.Context()
	in := mkInbox("full-store")
	if err := s.CreateInbox(ctx, in); err != nil {
		t.Errorf("CreateInbox: %v", err)
		return
	}
	for range 2 {
		if _, _, err := s.Append(ctx, in.ID, nm()); err != nil {
			t.Errorf("Append before the Store was full: %v", err)
			return
		}
	}

	undo := makeFull()
	big := nm(func(m *inbox.NewMessage) { m.Body = make([]byte, 1<<20) })
	for i := range 3 {
		msg, created, err := s.Append(ctx, in.ID, big)
		switch {
		case err == nil:
			t.Errorf("Append %d to a full Store stored a message (seq %d): MakeFull must leave no room for a 1 MiB body", i+1, msg.Seq)
			return
		case !errors.Is(err, inbox.ErrStorageFull):
			t.Errorf("Append %d to a full Store: %v; want an error that wraps inbox.ErrStorageFull", i+1, err)
		case msg != nil || created:
			t.Errorf("Append %d to a full Store returned a message (%v) or created = %v alongside its error", i+1, msg, created)
		}
	}
	// Reads that still work say the same thing: nothing was stored.
	if seq, err := s.LatestSeq(ctx, in.ID); err == nil && seq != 2 {
		t.Errorf("LatestSeq is %d after refused appends, want 2: a refused Append used up a sequence number or left a message", seq)
	}
	if undo == nil {
		return
	}
	undo()

	if seq, err := s.LatestSeq(ctx, in.ID); err != nil || seq != 2 {
		t.Errorf("LatestSeq is %d (%v) once there is room again, want 2: a refused Append used up a sequence number or left a message", seq, err)
	}
	if st, err := s.Stats(ctx, in.ID); err != nil || st.MessageCount != 2 {
		t.Errorf("Stats after refused appends: %+v, %v; want two messages", st, err)
	}
	msg, created, err := s.Append(ctx, in.ID, nm())
	switch {
	case err != nil:
		t.Errorf("Append once there is room again: %v", err)
	case !created || msg.Seq != 3:
		t.Errorf("the first message once there is room again has seq %d (created %v); want 3, with no number used up by a refused Append", msg.Seq, created)
	}
	if page, err := s.Query(ctx, in.ID, inbox.Query{Limit: inbox.MaxQueryLimit}); err != nil {
		t.Errorf("Query: %v", err)
	} else if got := slices.Sorted(slices.Values(seqsOf(page.Messages))); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("the inbox holds seqs %v after the refused appends, want [1 2 3]", got)
	}
}

// testConcurrentPrune calls Prune from several goroutines at once, on s and on
// any peers (other Store values on the same backend), while the inboxes it prunes
// are being appended to.
func testConcurrentPrune(t *testing.T, s inbox.Store, clk *Clock, peers []inbox.Store) {
	const inboxes, perInbox, keep, callers = 4, 60, 10, 8
	stores := append([]inbox.Store{s}, peers...)
	var made []*inbox.Inbox
	for i := range inboxes {
		in := createInbox(t, s, fmt.Sprintf("prune-concurrent-%d", i), func(in *inbox.Inbox) { in.RetentionMaxCount = keep })
		appendN(t, s, in.ID, perInbox, nil)
		made = append(made, in)
	}

	// Appends to the first inbox carry on while the calls run; they must still be
	// numbered without a gap.
	var wg sync.WaitGroup
	appended := make(chan int64, 100)
	wg.Go(func() {
		defer close(appended)
		for range 20 {
			m, _, err := s.Append(t.Context(), made[0].ID, nm())
			if err != nil {
				t.Errorf("Append during Prune: %v", err)
				return
			}
			appended <- m.Seq
		}
	})
	errs := make(chan error, callers*2)
	for i := range callers {
		wg.Go(func() {
			for range 2 {
				if _, err := stores[i%len(stores)].Prune(t.Context(), clk.Now()); err != nil {
					errs <- fmt.Errorf("call %d of Prune (store %d of %d): %w", i, i%len(stores), len(stores), err)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	next := int64(perInbox)
	for seq := range appended {
		next++
		if seq != next {
			t.Errorf("an append during the Prunes got seq %d, want %d", seq, next)
			next = seq
		}
	}

	// One more call leaves each inbox with exactly its newest messages, and the
	// one after that finds nothing to do.
	if _, err := s.Prune(t.Context(), clk.Now()); err != nil {
		t.Fatal(err)
	}
	for i, in := range made {
		last := int64(perInbox)
		if i == 0 {
			last = next
		}
		st, err := s.Stats(t.Context(), in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.LatestSeq != last || st.MessageCount != keep || st.OldestSeq != last-keep+1 {
			t.Errorf("%s: stats %+v after the concurrent Prunes; want the newest %d messages, seqs %d..%d", in.Slug, st, keep, last-keep+1, last)
		}
		if got := walkAscending(t, s, in.ID, inbox.Query{}, inbox.MaxQueryLimit); !slices.Equal(got, seqRange(last-keep+1, last)) {
			t.Errorf("%s: holds seqs %v, want %d..%d with none missing", in.Slug, got, last-keep+1, last)
		}
	}
	if n, err := s.Prune(t.Context(), clk.Now()); err != nil || n != 0 {
		t.Errorf("a Prune after the others deleted %d (%v); want 0", n, err)
	}
}

// testPruneRacesARetentionChange lowers an inbox's RetentionMaxCount while a Prune
// runs. Whichever the Prune applies, the inbox keeps a run of its newest messages
// that includes everything both limits keep, and the next Prune applies the new
// limit.
func testPruneRacesARetentionChange(t *testing.T, s inbox.Store, clk *Clock, peers []inbox.Store) {
	const rounds, perInbox, oldLimit, newLimit = 5, 50, 40, 10
	stores := append([]inbox.Store{s}, peers...)
	for r := range rounds {
		in := createInbox(t, s, fmt.Sprintf("prune-race-%d", r), func(in *inbox.Inbox) { in.RetentionMaxCount = oldLimit })
		appendN(t, s, in.ID, perInbox, nil)

		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := stores[r%len(stores)].Prune(t.Context(), clk.Now()); err != nil {
				t.Errorf("round %d: Prune racing UpdateInbox: %v", r, err)
			}
		})
		wg.Go(func() {
			for range 5 {
				cur, err := s.GetInbox(t.Context(), in.ID)
				if err != nil {
					t.Errorf("round %d: GetInbox: %v", r, err)
					return
				}
				cur.RetentionMaxCount = newLimit
				err = s.UpdateInbox(t.Context(), cur)
				if err == nil {
					return
				}
				if !errors.Is(err, inbox.ErrConflict) {
					t.Errorf("round %d: UpdateInbox racing Prune: %v", r, err)
					return
				}
			}
			t.Errorf("round %d: UpdateInbox kept conflicting with a Prune", r)
		})
		wg.Wait()

		st, err := s.Stats(t.Context(), in.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.LatestSeq != perInbox || st.MessageCount < newLimit || st.MessageCount > oldLimit || st.OldestSeq != perInbox-st.MessageCount+1 {
			t.Errorf("round %d: stats %+v after Prune raced the change; want the newest %d to %d messages, in a run ending at seq %d", r, st, newLimit, oldLimit, perInbox)
		}
		if _, err := s.Prune(t.Context(), clk.Now()); err != nil {
			t.Fatal(err)
		}
		if st, err := s.Stats(t.Context(), in.ID); err != nil || st.MessageCount != newLimit || st.OldestSeq != perInbox-newLimit+1 {
			t.Errorf("round %d: stats %+v (%v) after the next Prune; want the newest %d messages", r, st, err, newLimit)
		}
	}
}
