// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

var _ inbox.Backupper = (*Store)(nil)

const (
	backupPrefix = "backup-"
	backupSuffix = ".tmp"
	// staleBackup is how old a leftover snapshot file must be before Open deletes
	// it. A snapshot is removed when it is closed, so one that is still here was
	// left by a process that died; the wait keeps Open from deleting a snapshot
	// another process is reading.
	staleBackup = time.Hour
)

// Backup implements inbox.Backupper.
//
// It writes the snapshot with VACUUM INTO, which copies the database in one
// read transaction: a consistent view as of one moment, taken while appends
// carry on in the WAL. The copy is a complete, standalone database file with no
// -wal or -shm beside it. It is written to the data directory, the one place
// that is known to be on the same volume and private to the owner, so the
// directory needs free space for a second copy of the database.
func (s *Store) Backup(ctx context.Context) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if tenant, scoped := inbox.TenantFrom(ctx); scoped {
		// The snapshot holds every tenant's inboxes, messages and tokens.
		return nil, 0, fmt.Errorf("%w: a backup holds every tenant's data, so it cannot be taken for tenant %q", inbox.ErrForbidden, tenant)
	}
	if s.closed.Load() {
		return nil, 0, errors.New("sqlite: backup: the store is closed")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), backupPrefix+"*"+backupSuffix)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: backup: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		removeSnapshot(name)
		return nil, 0, fmt.Errorf("sqlite: backup: %w", err)
	}
	// VACUUM INTO needs a file that does not exist or is empty; this one is empty.
	// The read pool is read-only, which VACUUM refuses even though it changes
	// nothing in the source, and the writer must stay free for appends, so the
	// copy gets a connection of its own. It reads a snapshot and writes only the
	// new file, so it never takes the write lock.
	conn, err := openDB(s.path, 1, false)
	if err != nil {
		removeSnapshot(name)
		return nil, 0, err
	}
	conn.SetMaxOpenConns(1)
	_, err = conn.ExecContext(ctx, `VACUUM INTO ?`, name)
	_ = conn.Close()
	if err != nil {
		removeSnapshot(name)
		return nil, 0, s.ctxErr(ctx, fmt.Errorf("sqlite: backup: %w", err))
	}
	if err := renewEpochs(ctx, name); err != nil {
		removeSnapshot(name)
		return nil, 0, s.ctxErr(ctx, fmt.Errorf("sqlite: backup: %w", err))
	}
	snap, err := os.Open(name) //nolint:gosec // a file this function just created in the data directory
	if err != nil {
		removeSnapshot(name)
		return nil, 0, fmt.Errorf("sqlite: backup: %w", err)
	}
	st, err := snap.Stat()
	if err != nil {
		_ = snap.Close()
		removeSnapshot(name)
		return nil, 0, fmt.Errorf("sqlite: backup: %w", err)
	}
	return &snapshotFile{File: snap}, st.Size(), nil
}

// renewEpochs gives every inbox in a snapshot a new epoch. A server started on a
// restored snapshot may accept different messages from the one the snapshot was
// taken from, so the sequence numbers it hands out do not mean what the original
// meant, and a cursor from one must not be taken for a cursor of the other.
func renewEpochs(ctx context.Context, path string) error {
	conn, err := openDB(path, 1, false)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)
	// The readers go with the old epochs: a position is a sequence number of one epoch.
	// Both statements are one transaction, so a snapshot never has one without the other.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("renew epochs: %w", err)
	}
	for _, stmt := range []string{`DELETE FROM readers`, `UPDATE inboxes SET epoch = lower(hex(randomblob(16)))`} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("renew epochs: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("renew epochs: %w", err)
	}
	// Back to a single self-contained file, as VACUUM INTO left it.
	if _, err := conn.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
		return fmt.Errorf("renew epochs: %w", err)
	}
	return nil
}

// snapshotJournals are the files SQLite may keep beside a snapshot while it is being
// written: the rollback journal VACUUM INTO and the epoch update use, and the log and
// its index if the file was ever in WAL mode.
var snapshotJournals = []string{"-journal", "-wal", "-shm"}

// removeSnapshot deletes a snapshot file and anything SQLite left beside it. A write
// that failed for lack of room leaves a journal behind, and nothing but this would
// ever remove it.
func removeSnapshot(name string) {
	_ = os.Remove(name)
	for _, suffix := range snapshotJournals {
		_ = os.Remove(name + suffix)
	}
}

// snapshotFile deletes its file when closed.
type snapshotFile struct{ *os.File }

func (f *snapshotFile) Close() error {
	err := f.File.Close()
	if rmErr := os.Remove(f.Name()); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = rmErr
	}
	for _, suffix := range snapshotJournals {
		_ = os.Remove(f.Name() + suffix)
	}
	return err
}

// removeStaleBackups deletes snapshots left behind by a process that died.
func removeStaleBackups(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		// Beside a snapshot that died while its epochs were being renewed there may
		// be the journal files SQLite keeps for it.
		base := n
		for _, suffix := range snapshotJournals {
			base = strings.TrimSuffix(base, suffix)
		}
		if e.IsDir() || !strings.HasPrefix(base, backupPrefix) || !strings.HasSuffix(base, backupSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > staleBackup {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}
