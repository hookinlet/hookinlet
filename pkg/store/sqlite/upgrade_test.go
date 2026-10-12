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
// every inbox gets a random ingest key, what a "none" inbox accepted is
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

	// An ingest key is a capability, so inboxes that had their slug for a key now have
	// a random one, and the slug is no longer a way in.
	keys := map[string]bool{}
	for _, slug := range []string{"signed", "open"} {
		in, err := s.GetInboxBySlug(t.Context(), slug)
		if err != nil {
			t.Fatal(err)
		}
		if in.IngestKey == slug || len(in.IngestKey) != 32 || keys[in.IngestKey] {
			t.Errorf("the key of %q is %q", slug, in.IngestKey)
		}
		keys[in.IngestKey] = true
		if r, err := s.ResolveIngestKey(t.Context(), in.IngestKey); err != nil || r.Slug != slug {
			t.Errorf("ResolveIngestKey(%q) = %+v, %v", in.IngestKey, r, err)
		}
		if _, err := s.ResolveIngestKey(t.Context(), slug); err == nil {
			t.Errorf("the slug %q is still a way in", slug)
		}
	}
	// Each inbox's token (the hash stored in its row) still resolves, now as a token
	// named "default" scoped to that inbox alone.
	for i, slug := range []string{"signed", "open"} {
		in, _ := s.GetInboxBySlug(t.Context(), slug)
		tk, err := s.GetTokenByHash(t.Context(), []byte{byte(i + 1)})
		if err != nil {
			t.Errorf("the token of %q after the upgrade: %v", slug, err)
			continue
		}
		if tk.Name != "default" || tk.AllInboxes || len(tk.InboxIDs) != 1 || tk.InboxIDs[0] != in.ID {
			t.Errorf("the token of %q = %+v, want \"default\" scoped to inbox %d", slug, tk, in.ID)
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

// Before an existing database is migrated, a copy of it as it was is kept.
func TestADatabaseIsCopiedBeforeItIsMigrated(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_init.sql", "0002_delivery_index.sql"} {
		b, err := os.ReadFile(filepath.Join("migrations", name)) //nolint:gosec // this package's own migration files
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(t.Context(), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`INSERT INTO schema_migrations VALUES (1, 'then'), (2, 'then')`,
		`INSERT INTO inboxes (slug, name, verifier, agent_token_hash, created_at, updated_at) VALUES ('kept', 'kept', 'none', x'01', '2026-01-01T00:00:00.000000Z', '2026-01-01T00:00:00.000000Z')`,
	} {
		if _, err := raw.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()

	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	snap := filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"2.bak")
	info, err := os.Stat(snap)
	if err != nil {
		t.Fatalf("no copy was kept: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the copy's mode is %v, want 0600", info.Mode().Perm())
	}
	old, err := sql.Open("sqlite", snap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	var version, inboxes int
	if err := old.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 2 {
		t.Errorf("the copy is at version %d (%v), want the version before the upgrade, 2", version, err)
	}
	if err := old.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM inboxes`).Scan(&inboxes); err != nil || inboxes != 1 {
		t.Errorf("the copy holds %d inboxes (%v)", inboxes, err)
	}

	// Nothing pending, nothing copied: the same copy stays, and no other appears.
	before, _ := os.Stat(snap)
	s, err = sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	after, _ := os.Stat(snap)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a start with nothing to migrate replaced the copy")
	}
	if all, _ := filepath.Glob(filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"*")); len(all) != 1 {
		t.Errorf("copies: %v", all)
	}
}

func TestANewDatabaseIsNotCopied(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if all, _ := filepath.Glob(filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"*")); len(all) != 0 {
		t.Errorf("a new database left %v", all)
	}
}

// The body is the last column of a message, and the rebuild keeps every message,
// its id, and its indexes.
func TestTheBodyIsTheLastColumnAfterTheUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	in := &inbox.Inbox{Slug: "body-last", IngestKey: "body-last", Name: "n", Verifier: "none"}
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("kept"), Verified: true, DeliveryID: "d1"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	db, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), `SELECT name FROM pragma_table_info('messages') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	_ = rows.Close()
	if len(cols) == 0 || cols[len(cols)-1] != "body" {
		t.Errorf("columns: %v; the body must come last", cols)
	}
	for _, idx := range []string{"messages_inbox_type", "messages_inbox_received", "messages_dedupe", "messages_inbox_delivery"} {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d (%v)", idx, n, err)
		}
	}
}
