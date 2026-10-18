// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
)

// The conformance suite, with the two checks that need the Store's own help: a
// full Store (SQLite is given no room to grow, the error it gives for a full
// disk), and a second Store value on the same data directory, which Prune is
// called on at the same time.
func TestStoreSuite(t *testing.T) {
	inboxtest.StoreSuiteWith(t, func(t *testing.T, now func() time.Time) inbox.Store {
		s, err := Open(t.Context(), t.TempDir(), WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}, inboxtest.StoreOptions{
		MakeFull: func(t *testing.T, s inbox.Store) func() {
			st := s.(*Store)
			var pages int
			if err := st.wdb.QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&pages); err != nil {
				t.Fatal(err)
			}
			if _, err := st.wdb.ExecContext(t.Context(), `PRAGMA max_page_count = `+strconv.Itoa(pages)); err != nil {
				t.Fatal(err)
			}
			return func() {
				if _, err := st.wdb.ExecContext(t.Context(), `PRAGMA max_page_count = 1073741823`); err != nil {
					t.Fatal(err)
				}
			}
		},
		Peer: func(t *testing.T, s inbox.Store) inbox.Store {
			peer, err := Open(t.Context(), filepath.Dir(s.(*Store).path))
			if err != nil {
				t.Fatal(err)
			}
			return peer
		},
	})
}
