// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
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
	writeVersion2Database(t, dir)

	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	checkCopy(t, dir)
}

func writeVersion2Database(t *testing.T, dir string) {
	t.Helper()
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
}

func checkCopy(t *testing.T, dir string) {
	t.Helper()
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
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
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
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
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
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
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

// An upgrade that was cut short leaves the copy of the version it started from; the
// next start must not replace it with a copy of a half-upgraded database.
func TestARetriedUpgradeKeepsTheCopyItAlreadyHas(t *testing.T) {
	dir := t.TempDir()
	writeVersion2Database(t, dir)
	first := filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"1.bak")
	if err := os.WriteFile(first, []byte("the copy from before the first attempt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sqlite.DBFileName+".migrating"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	b, err := os.ReadFile(first) //nolint:gosec // a path in this test's own temp directory
	if err != nil || string(b) != "the copy from before the first attempt" {
		t.Errorf("the earlier copy: %q, %v", b, err)
	}
	if all, _ := filepath.Glob(filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"*")); len(all) != 1 {
		t.Errorf("copies: %v", all)
	}
	if _, err := os.Stat(filepath.Join(dir, sqlite.DBFileName+".migrating")); err == nil {
		t.Error("the marker outlived the upgrade")
	}
}

// A copy that cannot be made takes nothing from the operator, and the database is
// left as it was.
func TestAFailedCopyLeavesTheEarlierOneAndTheDatabaseAlone(t *testing.T) {
	dir := t.TempDir()
	writeVersion2Database(t, dir)
	earlier := filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"1.bak")
	if err := os.WriteFile(earlier, []byte("earlier"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Something in the way of the new copy that cannot be removed.
	blocker := filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"2.bak.partial")
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock()); err == nil {
		t.Fatal("Open migrated without a copy")
	}
	b, err := os.ReadFile(earlier) //nolint:gosec // a path in this test's own temp directory
	if err != nil || string(b) != "earlier" {
		t.Errorf("the earlier copy: %q, %v", b, err)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil || v != 2 {
		t.Errorf("the database is at version %d (%v), want it untouched at 2", v, err)
	}
}

// The write-ahead log is cut back to its limit after it is checkpointed, and does
// not stay as large as the biggest burst of writes.
func TestTheWriteAheadLogIsBounded(t *testing.T) {
	dir := t.TempDir()
	const limit = 1 << 20
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithWALLimit(limit))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	in := &inbox.Inbox{Slug: "wal-bound", IngestKey: "wal-bound", Name: "n", Verifier: "none", MaxBodyBytes: 4 << 20}
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 3<<20) // one message much bigger than the limit
	for range 6 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: body, Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	// A burst of small writes lets the checkpoint run and the log reset.
	for range 200 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, sqlite.DBFileName+"-wal"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 4*limit {
		t.Errorf("the write-ahead log is %d bytes after the burst, want it cut back near %d", info.Size(), limit)
	}
}

// Without the lock, a database that needs migrating is refused and left as it was:
// the command line may look at a server's data, but not upgrade it from underneath.
func TestAnOpenWithoutTheLockDoesNotMigrate(t *testing.T) {
	dir := t.TempDir()
	writeVersion2Database(t, dir)
	_, err := sqlite.Open(t.Context(), dir)
	var need *sqlite.NeedsMigrationError
	if !errors.As(err, &need) || need.From != 2 {
		t.Fatalf("opening an old database without the lock: %v", err)
	}
	if all, _ := filepath.Glob(filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"*")); len(all) != 0 {
		t.Errorf("a copy was made: %v", all)
	}
	if _, err := os.Stat(filepath.Join(dir, sqlite.DBFileName+".migrating")); err == nil {
		t.Error("a marker was left")
	}
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = sqlite.Open(t.Context(), dir) // now current: no lock needed
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}

// An upgrade that was rolled back (the copy put back over the database, the marker
// left) must not keep that stale copy for the next attempt.
func TestARolledBackUpgradeGetsAFreshCopyNextTime(t *testing.T) {
	dir := t.TempDir()
	writeVersion2Database(t, dir)
	stale := filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"2.bak")
	if err := os.WriteFile(stale, []byte("a copy from before deliveries arrived"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sqlite.DBFileName+".migrating"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	b, err := os.ReadFile(stale) //nolint:gosec // a path in this test's own temp directory
	if err != nil || string(b) == "a copy from before deliveries arrived" {
		t.Errorf("the stale copy was kept: %q, %v", b, err)
	}
}

// An upgrade killed before its first migration committed finds the data as the copy
// left it, and keeps that copy rather than copying again on every restart.
func TestARestartedUpgradeKeepsACopyOfUnchangedData(t *testing.T) {
	dir := t.TempDir()
	writeVersion2Database(t, dir)
	raw, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	var stamp string
	if err := raw.QueryRowContext(t.Context(), `SELECT (SELECT COALESCE(MAX(id),0) FROM messages) || '/' ||
		(SELECT COUNT(*) FROM messages) || '/' || (SELECT COUNT(*) FROM inboxes) || '/' ||
		(SELECT COALESCE(SUM(version),0) FROM inboxes) || '/' || (SELECT COALESCE(MAX(id),0) FROM inboxes)`).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	kept := filepath.Join(dir, sqlite.MigrationSnapshotPrefix+"2.bak")
	if err := os.WriteFile(kept, []byte("made before the first attempt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sqlite.DBFileName+".migrating"), []byte("2 "+stamp+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if b, err := os.ReadFile(kept); err != nil || string(b) != "made before the first attempt" { //nolint:gosec // this test's own temp directory
		t.Errorf("the copy was replaced: %q, %v", b, err)
	}
}

// Nil arguments are errors, not panics.
func TestNilArgumentsAreRefused(t *testing.T) {
	s, err := sqlite.Open(t.Context(), t.TempDir(), nil, sqlite.WithClock(nil), sqlite.WithFreeSpace(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := t.Context()
	if err := s.CreateInbox(ctx, nil); !errors.Is(err, inbox.ErrInvalid) {
		t.Errorf("CreateInbox(nil): %v", err)
	}
	if err := s.UpdateInbox(ctx, nil); !errors.Is(err, inbox.ErrInvalid) {
		t.Errorf("UpdateInbox(nil): %v", err)
	}
	if err := s.CreateToken(ctx, nil); !errors.Is(err, inbox.ErrInvalid) {
		t.Errorf("CreateToken(nil): %v", err)
	}
	if _, _, err := s.Append(ctx, 1, nil); !errors.Is(err, inbox.ErrInvalid) {
		t.Errorf("Append(nil): %v", err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Errorf("Ping with a nil free-space function: %v", err)
	}
}

// A process with the database open that takes no lock (the previous release) is not
// migrated underneath.
func TestAMigrationRefusesWhileAnotherProcessHasTheDatabaseOpen(t *testing.T) {
	dir := t.TempDir()
	writeVersion2Database(t, dir)
	other, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(200)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	var n int
	if err := other.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM inboxes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock()); err == nil {
		t.Fatal("migrated while another connection had the database open")
	}
	var v int
	if err := other.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil || v != 2 {
		t.Errorf("schema version %d, %v; want it left at 2", v, err)
	}
	_ = other.Close()
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatalf("once the other connection is gone: %v", err)
	}
	_ = s.Close()
}

// A database copied in with a wide mode (a restore with the usual umask) is made
// private when it is opened, along with the files SQLite keeps beside it.
func TestOpeningTightensTheModeOfADatabaseThatCameFromElsewhere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful here")
	}
	dir := t.TempDir()
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	db := filepath.Join(dir, sqlite.DBFileName)
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(p, 0o644); err != nil { //nolint:gosec // the point of the test: a wide mode
			t.Fatal(err)
		}
	}
	s, err = sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // SQLite removes them when the last connection closes
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v", filepath.Base(p), fi.Mode().Perm())
		}
	}
}
