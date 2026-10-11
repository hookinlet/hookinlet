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
	if s.closed.Load() {
		return nil, 0, errors.New("sqlite: backup: the store is closed")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), backupPrefix+"*"+backupSuffix)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: backup: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return nil, 0, fmt.Errorf("sqlite: backup: %w", err)
	}
	// VACUUM INTO needs a file that does not exist or is empty; this one is empty.
	// The read pool is read-only, which VACUUM refuses even though it changes
	// nothing in the source, and the writer must stay free for appends, so the
	// copy gets a connection of its own. It reads a snapshot and writes only the
	// new file, so it never takes the write lock.
	conn, err := openDB(s.path, 1, false)
	if err != nil {
		_ = os.Remove(name)
		return nil, 0, err
	}
	conn.SetMaxOpenConns(1)
	_, err = conn.ExecContext(ctx, `VACUUM INTO ?`, name)
	_ = conn.Close()
	if err != nil {
		_ = os.Remove(name)
		return nil, 0, s.ctxErr(ctx, fmt.Errorf("sqlite: backup: %w", err))
	}
	if err := renewEpochs(ctx, name); err != nil {
		_ = os.Remove(name)
		return nil, 0, s.ctxErr(ctx, fmt.Errorf("sqlite: backup: %w", err))
	}
	snap, err := os.Open(name) //nolint:gosec // a file this function just created in the data directory
	if err != nil {
		_ = os.Remove(name)
		return nil, 0, fmt.Errorf("sqlite: backup: %w", err)
	}
	st, err := snap.Stat()
	if err != nil {
		_ = snap.Close()
		_ = os.Remove(name)
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
	if _, err := conn.ExecContext(ctx, `UPDATE inboxes SET epoch = lower(hex(randomblob(16)))`); err != nil {
		return fmt.Errorf("renew epochs: %w", err)
	}
	// Back to a single self-contained file, as VACUUM INTO left it.
	if _, err := conn.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
		return fmt.Errorf("renew epochs: %w", err)
	}
	return nil
}

// snapshotFile deletes its file when closed.
type snapshotFile struct{ *os.File }

func (f *snapshotFile) Close() error {
	err := f.File.Close()
	if rmErr := os.Remove(f.Name()); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = rmErr
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
		if e.IsDir() || !strings.HasPrefix(n, backupPrefix) || !strings.HasSuffix(n, backupSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > staleBackup {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}
