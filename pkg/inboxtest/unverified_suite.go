// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// UnverifiedCounterSuite runs the inbox.UnverifiedCounter conformance tests.
//
// newStore must return a new, empty Store on every call, driven by the given
// clock, exactly as for StoreSuite. The Store must implement
// inbox.UnverifiedCounter. The suite closes each Store itself.
func UnverifiedCounterSuite(t *testing.T, newStore func(t *testing.T, now func() time.Time) inbox.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, inbox.UnverifiedCounter, inbox.Store, *Clock)
	}{
		{"CountsOnlyUnverified", testCountsOnlyUnverified},
		{"CountIsBoundedByTheLimit", testCountIsBounded},
		{"CountIsPerInbox", testCountIsPerInbox},
		{"RetentionLowersTheCount", testRetentionLowersUnverifiedCount},
		{"MissingInbox", testCountMissingInbox},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := NewClock(baseTime)
			s := newStore(t, clk.Now)
			t.Cleanup(func() { _ = s.Close() })
			uc, ok := s.(inbox.UnverifiedCounter)
			if !ok {
				t.Fatalf("%T does not implement inbox.UnverifiedCounter", s)
			}
			c.fn(t, uc, s, clk)
		})
	}
}

func unverified(m *inbox.NewMessage) { m.Verified = false; m.VerifyError = "signature mismatch" }

func countUnverified(t *testing.T, uc inbox.UnverifiedCounter, id inbox.ID, limit int64) int64 {
	t.Helper()
	n, err := uc.CountUnverified(t.Context(), id, limit)
	if err != nil {
		t.Fatalf("CountUnverified: %v", err)
	}
	return n
}

func testCountsOnlyUnverified(t *testing.T, uc inbox.UnverifiedCounter, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "count")
	if n := countUnverified(t, uc, in.ID, 100); n != 0 {
		t.Fatalf("an empty inbox counts %d", n)
	}
	appendN(t, s, in.ID, 3, func(int, *inbox.NewMessage) {})
	if n := countUnverified(t, uc, in.ID, 100); n != 0 {
		t.Fatalf("verified messages were counted: %d", n)
	}
	appendN(t, s, in.ID, 2, func(int, *inbox.NewMessage) {}) // more verified, interleaved below
	appendN(t, s, in.ID, 4, func(_ int, m *inbox.NewMessage) { unverified(m) })
	appendMsg(t, s, in.ID)
	if n := countUnverified(t, uc, in.ID, 100); n != 4 {
		t.Fatalf("counted %d unverified, want 4", n)
	}
}

func testCountIsBounded(t *testing.T, uc inbox.UnverifiedCounter, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "bounded")
	appendN(t, s, in.ID, 5, func(_ int, m *inbox.NewMessage) { unverified(m) })
	for _, c := range []struct{ limit, want int64 }{{1, 1}, {4, 4}, {5, 5}, {6, 5}, {1000, 5}} {
		if n := countUnverified(t, uc, in.ID, c.limit); n != c.want {
			t.Errorf("limit %d: counted %d, want %d", c.limit, n, c.want)
		}
	}
}

func testCountIsPerInbox(t *testing.T, uc inbox.UnverifiedCounter, s inbox.Store, _ *Clock) {
	a, b := createInbox(t, s, "inbox-a"), createInbox(t, s, "inbox-b")
	appendN(t, s, a.ID, 3, func(_ int, m *inbox.NewMessage) { unverified(m) })
	appendN(t, s, b.ID, 1, func(_ int, m *inbox.NewMessage) { unverified(m) })
	if x, y := countUnverified(t, uc, a.ID, 100), countUnverified(t, uc, b.ID, 100); x != 3 || y != 1 {
		t.Fatalf("counted %d and %d, want 3 and 1", x, y)
	}
}

// Retention frees room under a cap: without this, a full inbox would stay full.
func testRetentionLowersUnverifiedCount(t *testing.T, uc inbox.UnverifiedCounter, s inbox.Store, clk *Clock) {
	in := createInbox(t, s, "pruned", func(in *inbox.Inbox) { in.RetentionDays = 1 })
	appendN(t, s, in.ID, 3, func(_ int, m *inbox.NewMessage) { unverified(m) })
	clk.Set(baseTime.Add(48 * time.Hour))
	appendN(t, s, in.ID, 1, func(_ int, m *inbox.NewMessage) { unverified(m) })
	if _, err := s.Prune(t.Context(), clk.Now()); err != nil {
		t.Fatal(err)
	}
	if n := countUnverified(t, uc, in.ID, 100); n != 1 {
		t.Fatalf("counted %d after pruning, want 1", n)
	}
}

func testCountMissingInbox(t *testing.T, uc inbox.UnverifiedCounter, _ inbox.Store, _ *Clock) {
	_, err := uc.CountUnverified(t.Context(), 12345, 10)
	isErr(t, "CountUnverified on a missing inbox", err, inbox.ErrNotFound)
}
