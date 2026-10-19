// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite_test

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// A snapshot is restored as another server, which may accept different messages, so the
// positions of the named readers (sequence numbers of the old epoch) must not be in it,
// while the live store keeps its own.
func TestABackupHasNoReaderPositions(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	in := newInbox("readers-backup")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AckReader(t.Context(), in.ID, "worker", in.Epoch, 2, 0); err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Backup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	restored := t.TempDir()
	if err := os.WriteFile(filepath.Join(restored, sqlite.DBFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(restored, sqlite.DBFileName)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM readers`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the snapshot holds %d reader positions (%v), want none: its inboxes have new epochs", n, err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 3 {
		t.Errorf("the snapshot holds %d messages (%v), want 3", n, err)
	}
	if got, err := s.Readers(t.Context(), in.ID); err != nil || len(got) != 1 {
		t.Errorf("the live store lost its reader by being backed up: %v, %v", got, err)
	}
}

// A database has the table, and no readers.
func TestANewDatabaseHasTheReadersTable(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	in := newInbox("readers-upgrade")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Readers(t.Context(), in.ID); err != nil || len(got) != 0 {
		t.Errorf("Readers on a new database = %v, %v", got, err)
	}
	v, err := s.SchemaVersion(t.Context())
	if err != nil || v < 11 {
		t.Errorf("schema version %d (%v), want at least 11", v, err)
	}
}

// A row whose epoch is not the inbox's is never returned, never counted against the cap, and
// does not outlive the next acknowledgement, whatever put it there.
func TestAReaderRowOfAnotherEpochIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	in := newInbox("readers-stale")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO readers (inbox_id, name, seq, epoch, updated_at) VALUES (?, 'old', 1, 'not-the-epoch', '2026-01-01T00:00:00.000000Z')`, int64(in.ID)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Readers(t.Context(), in.ID); err != nil || len(got) != 0 {
		t.Errorf("Readers = %v, %v; a row of another epoch must not be returned", got, err)
	}
	if _, err := s.AckReader(t.Context(), in.ID, "new", in.Epoch, 0, 1); err != nil {
		t.Errorf("AckReader with a cap of 1 refused for a row of another epoch: %v", err)
	}
	var n int
	if err := raw.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM readers WHERE name = 'old'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the row of another epoch is still there (%d, %v)", n, err)
	}
}
