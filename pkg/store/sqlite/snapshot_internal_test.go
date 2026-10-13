// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A snapshot that could not be written (the volume was full) leaves SQLite's journal
// beside it. Dropping the snapshot drops that too, or it stays in the data directory
// for good, holding pages of the database.
func TestRemovingASnapshotRemovesTheJournalBesideIt(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "backup-123.tmp")
	files := []string{name}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		files = append(files, name+suffix)
	}
	keep := filepath.Join(dir, "hookinlet.db")
	for _, f := range append(files, keep) {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removeSnapshot(name)
	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s is still there", filepath.Base(f))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the database was removed too: %v", err)
	}
}

// The journal that a snapshot which died while being written leaves beside it is
// swept at the next start like the snapshot itself, once it is old enough.
func TestOpenRemovesOldLeftoverSnapshotJournals(t *testing.T) {
	dir := t.TempDir()
	old := []string{"backup-old.tmp-journal", "backup-old.tmp-wal", "backup-old.tmp-shm"}
	fresh := "backup-fresh.tmp-journal"
	other := "notes.tmp-journal"
	for _, f := range append(append([]string{}, old...), fresh, other) {
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if f != fresh && f != other {
			past := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(p, past, past); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for _, f := range old {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed", f)
		}
	}
	for _, f := range []string{fresh, other} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s was removed: %v", f, err)
		}
	}
}
