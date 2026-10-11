// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// A database written by an earlier version keeps working after the upgrade:
// every inbox is still reached at the URL it had, what a "none" inbox accepted is
// no longer called verified, and what was already deduplicated stays so.
func TestAnEarlierDatabaseUpgrades(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	// The schema as migrations 1 and 2 left it, with the bookkeeping the runner keeps.
	for _, name := range []string{"0001_init.sql", "0002_delivery_index.sql"} {
		b, err := os.ReadFile(filepath.Join("migrations", name)) //nolint:gosec // this package's own migration files
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(t.Context(), string(b)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`INSERT INTO schema_migrations VALUES (1, 'then'), (2, 'then')`,
		`INSERT INTO inboxes (slug, name, verifier, agent_token_hash, created_at, updated_at, last_seq)
		   VALUES ('signed', 'signed', 'github', x'01', '2026-01-01T00:00:00.000000Z', '2026-01-01T00:00:00.000000Z', 2), ('open', 'open', 'none', x'02', '2026-01-01T00:00:00.000000Z', '2026-01-01T00:00:00.000000Z', 1)`,
		`INSERT INTO messages (inbox_id, seq, received_at, method, headers, body, size, verified, delivery_id)
		   VALUES (1, 1, '2026-01-01T00:00:00.000000Z', 'POST', '{}', x'', 0, 1, 'd1'), (2, 1, '2026-01-01T00:00:00.000000Z', 'POST', '{}', x'', 0, 1, NULL)`,
		`INSERT INTO messages (inbox_id, seq, received_at, method, headers, body, size, verified, delivery_id)
		   VALUES (1, 2, '2026-01-01T00:00:00.000000Z', 'POST', '{}', x'', 0, 0, 'd1')`,
	} {
		if _, err := raw.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = raw.Close()

	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("opening the earlier database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Every inbox is still at its slug.
	for _, slug := range []string{"signed", "open"} {
		in, err := s.ResolveIngestKey(t.Context(), slug)
		if err != nil || in.Slug != slug || in.IngestKey != slug {
			t.Errorf("ResolveIngestKey(%q) = %+v, %v", slug, in, err)
		}
	}
	// What the "none" inbox took is not verified any more; the signed one's is as it was.
	for _, tc := range []struct {
		slug string
		seq  int64
		want bool
	}{{"open", 1, false}, {"signed", 1, true}, {"signed", 2, false}} {
		in, _ := s.GetInboxBySlug(t.Context(), tc.slug)
		m, err := s.Get(t.Context(), in.ID, tc.seq)
		if err != nil {
			t.Errorf("%s seq %d: %v", tc.slug, tc.seq, err)
			continue
		}
		if m.Verified != tc.want {
			t.Errorf("%s seq %d: verified = %v, want %v", tc.slug, tc.seq, m.Verified, tc.want)
		}
	}
	// A repeat of a delivery stored before the upgrade, with its id as the key, is still a duplicate.
	in, _ := s.GetInboxBySlug(t.Context(), "signed")
	dup, created, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Verified: true, DeliveryID: "d1"})
	if err != nil || created || dup.Seq != 1 {
		t.Errorf("a repeat of an old delivery: %+v created %v err %v", dup, created, err)
	}
}
