// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// ReaderTrackerSuite runs the inbox.ReaderTracker conformance tests.
//
// newStore must return a new, empty Store on every call, driven by the given
// clock, exactly as for StoreSuite. The Store must implement inbox.ReaderTracker.
// The suite closes each Store itself. The cases that need inbox.EpochRenewer or
// tenants run only if the Store has them.
func ReaderTrackerSuite(t *testing.T, newStore func(t *testing.T, now func() time.Time) inbox.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(testing.TB, inbox.Store, inbox.ReaderTracker, *Clock)
	}{
		{"APositionIsSetAndListedByName", checkReaderSetAndList},
		{"AnAckMayMoveBackwards", checkReaderMovesBackwards},
		{"TheSeqIsBetweenZeroAndTheNewest", checkReaderSeqBounds},
		{"TheNameIsValidated", checkReaderNames},
		{"AnotherEpochIsRefusedAndStoresNothing", checkReaderWrongEpoch},
		{"TheCapStopsNewReadersOnly", checkReaderCap},
		{"TheCapHoldsUnderConcurrentCalls", checkReaderCapConcurrent},
		{"AnUnknownInboxIsNotFound", checkReaderUnknownInbox},
		{"ForgettingOneLeavesTheOthers", checkReaderForget},
		{"RenewingEpochsDropsEveryPosition", checkReaderRenewEpochs},
		{"DeletingTheInboxDeletesItsReaders", checkReaderInboxDeleted},
		{"ReadersOfOneTenantAreInvisibleToAnother", checkReaderTenants},
		{"ACancelledContextChangesNothing", checkReaderCancelled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := NewClock(baseTime)
			s := newStore(t, clock.Now)
			t.Cleanup(func() { _ = s.Close() })
			r, ok := s.(inbox.ReaderTracker)
			if !ok {
				t.Fatalf("%T does not implement inbox.ReaderTracker", s)
			}
			c.fn(t, s, r, clock)
		})
	}
}

// readerInbox makes an inbox holding n messages and returns it with its epoch.
func readerInbox(t testing.TB, s inbox.Store, slug string, n int) *inbox.Inbox {
	t.Helper()
	in := mkInbox(slug)
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatalf("CreateInbox: %v", err)
	}
	for range n {
		if _, _, err := s.Append(t.Context(), in.ID, nm()); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := s.GetInbox(t.Context(), in.ID)
	if err != nil {
		t.Fatalf("GetInbox: %v", err)
	}
	return got
}

func readerNames(ps []inbox.ReaderPosition) string {
	var names []string
	for _, p := range ps {
		names = append(names, fmt.Sprintf("%s=%d", p.Name, p.Seq))
	}
	return strings.Join(names, ",")
}

func checkReaderSetAndList(t testing.TB, s inbox.Store, r inbox.ReaderTracker, clock *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-list", 5)
	if got, err := r.Readers(ctx, in.ID); err != nil || len(got) != 0 {
		t.Errorf("Readers of an inbox with none = %v, %v; want an empty list", got, err)
	}
	clock.Advance(time.Minute)
	at := clock.Now()
	got, err := r.AckReader(ctx, in.ID, "worker-b", in.Epoch, 3, 0)
	if err != nil {
		t.Errorf("AckReader: %v", err)
		return
	}
	if got.Name != "worker-b" || got.Seq != 3 || got.Epoch != in.Epoch || !got.UpdatedAt.Equal(at) {
		t.Errorf("AckReader returned %+v, want worker-b at seq 3, epoch %q, updated at %v", got, in.Epoch, at)
	}
	if _, err := r.AckReader(ctx, in.ID, "worker-a", in.Epoch, 5, 0); err != nil {
		t.Errorf("AckReader: %v", err)
	}
	list, err := r.Readers(ctx, in.ID)
	if err != nil || readerNames(list) != "worker-a=5,worker-b=3" {
		t.Errorf("Readers = %q, %v; want worker-a=5,worker-b=3 (ordered by name)", readerNames(list), err)
	}
	for _, p := range list {
		if p.Epoch != in.Epoch || p.UpdatedAt.IsZero() {
			t.Errorf("Readers returned %+v without the epoch %q or a time", p, in.Epoch)
		}
	}
	// A reader of another inbox is not listed here.
	other := readerInbox(t, s, "readers-list-other", 2)
	if _, err := r.AckReader(ctx, other.ID, "elsewhere", other.Epoch, 1, 0); err != nil {
		t.Errorf("AckReader: %v", err)
	}
	if list, _ := r.Readers(ctx, in.ID); readerNames(list) != "worker-a=5,worker-b=3" {
		t.Errorf("Readers of the first inbox = %q after another inbox got a reader", readerNames(list))
	}
}

func checkReaderMovesBackwards(t testing.TB, s inbox.Store, r inbox.ReaderTracker, clock *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-back", 5)
	if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, 4, 0); err != nil {
		t.Errorf("AckReader: %v", err)
	}
	clock.Advance(time.Minute)
	got, err := r.AckReader(ctx, in.ID, "a", in.Epoch, 1, 0)
	if err != nil || got.Seq != 1 || !got.UpdatedAt.Equal(clock.Now()) {
		t.Errorf("AckReader back to 1 = %+v, %v; want seq 1 updated at %v", got, err, clock.Now())
	}
	if list, _ := r.Readers(ctx, in.ID); readerNames(list) != "a=1" {
		t.Errorf("Readers = %q, want a=1 (one reader, moved back)", readerNames(list))
	}
}

func checkReaderSeqBounds(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-bounds", 3)
	for _, seq := range []int64{-1, 4, 1 << 40} {
		if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, seq, 0); !errors.Is(err, inbox.ErrInvalid) {
			t.Errorf("AckReader at seq %d (the newest is 3) = %v, want an error wrapping ErrInvalid", seq, err)
		}
	}
	if list, _ := r.Readers(ctx, in.ID); len(list) != 0 {
		t.Errorf("a refused ack stored %q", readerNames(list))
	}
	for _, seq := range []int64{0, 3} {
		if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, seq, 0); err != nil {
			t.Errorf("AckReader at seq %d: %v", seq, err)
		}
	}
	// An inbox that has never had a message accepts only 0.
	empty := readerInbox(t, s, "readers-bounds-empty", 0)
	if _, err := r.AckReader(ctx, empty.ID, "a", empty.Epoch, 0, 0); err != nil {
		t.Errorf("AckReader at 0 of an empty inbox: %v", err)
	}
	if _, err := r.AckReader(ctx, empty.ID, "a", empty.Epoch, 1, 0); !errors.Is(err, inbox.ErrInvalid) {
		t.Errorf("AckReader at 1 of an empty inbox = %v, want ErrInvalid", err)
	}
}

func checkReaderNames(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-names", 1)
	bad := []string{"", " ", "a b", "-a", ".a", "_a", "a/b", "a?b", "a%41", "é", "a\x00", "a\n", strings.Repeat("a", inbox.MaxReaderNameLen+1)}
	for _, name := range bad {
		if _, err := r.AckReader(ctx, in.ID, name, in.Epoch, 0, 0); !errors.Is(err, inbox.ErrInvalid) {
			t.Errorf("AckReader(%q) = %v, want an error wrapping ErrInvalid", name, err)
		}
	}
	for _, name := range []string{"a", "A", "0", "worker-1", "a.b_c-d", strings.Repeat("a", inbox.MaxReaderNameLen)} {
		if _, err := r.AckReader(ctx, in.ID, name, in.Epoch, 0, 0); err != nil {
			t.Errorf("AckReader(%q): %v", name, err)
		}
	}
	// Names are compared exactly: a and A are two readers.
	if list, _ := r.Readers(ctx, in.ID); len(list) != 6 {
		t.Errorf("Readers = %q, want 6 distinct readers", readerNames(list))
	}
}

func checkReaderWrongEpoch(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-epoch", 2)
	for _, epoch := range []string{"", "0123456789abcdef", in.Epoch + "0"} {
		if _, err := r.AckReader(ctx, in.ID, "a", epoch, 1, 0); !errors.Is(err, inbox.ErrEpochChanged) {
			t.Errorf("AckReader with epoch %q = %v, want an error wrapping ErrEpochChanged", epoch, err)
		}
	}
	if list, _ := r.Readers(ctx, in.ID); len(list) != 0 {
		t.Errorf("a refused ack stored %q", readerNames(list))
	}
}

func checkReaderCap(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-cap", 4)
	for _, name := range []string{"a", "b"} {
		if _, err := r.AckReader(ctx, in.ID, name, in.Epoch, 1, 2); err != nil {
			t.Errorf("AckReader(%s) under the cap: %v", name, err)
		}
	}
	if _, err := r.AckReader(ctx, in.ID, "c", in.Epoch, 1, 2); !errors.Is(err, inbox.ErrConflict) {
		t.Errorf("a third reader with a cap of 2 = %v, want an error wrapping ErrConflict", err)
	}
	if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, 4, 2); err != nil {
		t.Errorf("moving an existing reader at the cap: %v", err)
	}
	if list, _ := r.Readers(ctx, in.ID); readerNames(list) != "a=4,b=1" {
		t.Errorf("Readers = %q, want a=4,b=1", readerNames(list))
	}
	// Forgetting one makes room.
	if err := r.ForgetReader(ctx, in.ID, "b"); err != nil {
		t.Errorf("ForgetReader: %v", err)
	}
	if _, err := r.AckReader(ctx, in.ID, "c", in.Epoch, 1, 2); err != nil {
		t.Errorf("a new reader after one was forgotten: %v", err)
	}
	// The cap is per inbox.
	other := readerInbox(t, s, "readers-cap-other", 1)
	for _, name := range []string{"a", "b"} {
		if _, err := r.AckReader(ctx, other.ID, name, other.Epoch, 1, 2); err != nil {
			t.Errorf("another inbox's reader %s: %v", name, err)
		}
	}
	// A cap of zero or less is no cap.
	for i := range 7 {
		for _, max := range []int{0, -1} {
			if _, err := r.AckReader(ctx, other.ID, fmt.Sprintf("n%d-%d", i, max), other.Epoch, 1, max); err != nil {
				t.Errorf("AckReader with a cap of %d: %v", max, err)
			}
		}
	}
}

// The cap is checked as the position is written, so readers racing for the last
// places cannot take more than there are.
func checkReaderCapConcurrent(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-race", 1)
	const places, callers = 5, 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.AckReader(ctx, in.ID, fmt.Sprintf("r%d", i), in.Epoch, 1, places)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, inbox.ErrConflict):
				t.Errorf("AckReader: %v, want success or ErrConflict", err)
			}
		}()
	}
	wg.Wait()
	list, err := r.Readers(ctx, in.ID)
	if err != nil || len(list) != places || ok != places {
		t.Errorf("%d callers raced for %d places: %d succeeded and %d are stored (%v); want exactly %d", callers, places, ok, len(list), err, places)
	}
}

func checkReaderUnknownInbox(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	if _, err := r.AckReader(ctx, 987654, "a", "e", 0, 0); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("AckReader on an unknown inbox = %v, want ErrNotFound", err)
	}
	if _, err := r.Readers(ctx, 987654); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("Readers of an unknown inbox = %v, want ErrNotFound", err)
	}
	if err := r.ForgetReader(ctx, 987654, "a"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("ForgetReader on an unknown inbox = %v, want ErrNotFound", err)
	}
}

func checkReaderForget(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-forget", 3)
	for i, name := range []string{"a", "b", "c"} {
		if _, err := r.AckReader(ctx, in.ID, name, in.Epoch, int64(i+1), 0); err != nil {
			t.Errorf("AckReader: %v", err)
		}
	}
	if err := r.ForgetReader(ctx, in.ID, "b"); err != nil {
		t.Errorf("ForgetReader: %v", err)
	}
	if list, _ := r.Readers(ctx, in.ID); readerNames(list) != "a=1,c=3" {
		t.Errorf("Readers = %q, want a=1,c=3", readerNames(list))
	}
	if err := r.ForgetReader(ctx, in.ID, "b"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("forgetting a reader twice = %v, want ErrNotFound the second time", err)
	}
	if err := r.ForgetReader(ctx, in.ID, "never-was"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("forgetting a reader that never was = %v, want ErrNotFound", err)
	}
}

func checkReaderRenewEpochs(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	renewer, ok := s.(inbox.EpochRenewer)
	if !ok {
		t.Logf("the Store has no EpochRenewer; nothing to check")
		return
	}
	ctx := t.Context()
	in := readerInbox(t, s, "readers-renew", 3)
	if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, 2, 0); err != nil {
		t.Errorf("AckReader: %v", err)
		return
	}
	if err := renewer.RenewEpochs(ctx); err != nil {
		t.Errorf("RenewEpochs: %v", err)
		return
	}
	after, err := s.GetInbox(ctx, in.ID)
	if err != nil || after.Epoch == in.Epoch {
		t.Errorf("GetInbox after RenewEpochs: epoch %v (%v), was %q", after, err, in.Epoch)
		return
	}
	if list, err := r.Readers(ctx, in.ID); err != nil || len(list) != 0 {
		t.Errorf("Readers after RenewEpochs = %q, %v; a position belongs to one epoch, so every one is dropped", readerNames(list), err)
	}
	if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, 3, 0); !errors.Is(err, inbox.ErrEpochChanged) {
		t.Errorf("AckReader with the epoch from before = %v, want ErrEpochChanged", err)
	}
	if _, err := r.AckReader(ctx, in.ID, "a", after.Epoch, 3, 1); err != nil {
		t.Errorf("AckReader with the new epoch (a cap of 1: the dropped reader must not count): %v", err)
	}
}

func checkReaderInboxDeleted(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	in := readerInbox(t, s, "readers-deleted", 2)
	if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, 2, 0); err != nil {
		t.Errorf("AckReader: %v", err)
		return
	}
	if err := s.DeleteInbox(ctx, in.ID); err != nil {
		t.Errorf("DeleteInbox: %v", err)
		return
	}
	if _, err := r.Readers(ctx, in.ID); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("Readers of a deleted inbox = %v, want ErrNotFound", err)
	}
	// The slug is free again, and the new inbox starts with no readers (and, with
	// a cap of 1, with room).
	again := readerInbox(t, s, "readers-deleted", 2)
	if list, err := r.Readers(ctx, again.ID); err != nil || len(list) != 0 {
		t.Errorf("Readers of an inbox made again under the same slug = %q, %v; want none", readerNames(list), err)
	}
	if _, err := r.AckReader(ctx, again.ID, "a", again.Epoch, 1, 1); err != nil {
		t.Errorf("AckReader on the new inbox: %v", err)
	}
}

func checkReaderTenants(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	ctx := t.Context()
	mine := inbox.WithTenant(ctx, "readers-org")
	in := mkInbox("readers-tenant")
	if err := s.CreateInbox(mine, in); err != nil || in.Tenant != "readers-org" {
		t.Logf("the Store does not keep inboxes under tenants (%v); nothing to check", err)
		return
	}
	if _, _, err := s.Append(mine, in.ID, nm()); err != nil {
		t.Errorf("Append: %v", err)
		return
	}
	got, err := s.GetInbox(mine, in.ID)
	if err != nil {
		t.Errorf("GetInbox: %v", err)
		return
	}
	if _, err := r.AckReader(mine, in.ID, "a", got.Epoch, 1, 0); err != nil {
		t.Errorf("AckReader in the inbox's own tenant: %v", err)
	}
	theirs := inbox.WithTenant(ctx, "another-org")
	if _, err := r.AckReader(theirs, in.ID, "x", got.Epoch, 1, 0); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("AckReader from another tenant = %v, want ErrNotFound", err)
	}
	if _, err := r.Readers(theirs, in.ID); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("Readers from another tenant = %v, want ErrNotFound", err)
	}
	if err := r.ForgetReader(theirs, in.ID, "a"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("ForgetReader from another tenant = %v, want ErrNotFound", err)
	}
	if list, err := r.Readers(mine, in.ID); err != nil || readerNames(list) != "a=1" {
		t.Errorf("the tenant's Readers = %q, %v; want a=1 untouched", readerNames(list), err)
	}
}

func checkReaderCancelled(t testing.TB, s inbox.Store, r inbox.ReaderTracker, _ *Clock) {
	t.Helper()
	in := readerInbox(t, s, "readers-cancel", 2)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.AckReader(ctx, in.ID, "a", in.Epoch, 1, 0); err == nil {
		t.Errorf("AckReader with a cancelled context succeeded")
	}
	if _, err := r.Readers(ctx, in.ID); err == nil {
		t.Errorf("Readers with a cancelled context succeeded")
	}
	if err := r.ForgetReader(ctx, in.ID, "a"); err == nil {
		t.Errorf("ForgetReader with a cancelled context succeeded")
	}
	if list, _ := r.Readers(t.Context(), in.ID); len(list) != 0 {
		t.Errorf("a cancelled AckReader stored %q", readerNames(list))
	}
}
