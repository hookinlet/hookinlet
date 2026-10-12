// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	msqlite "modernc.org/sqlite"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// sqliteFull is SQLITE_FULL: the database or the disk it is on has no room.
const sqliteFull = 13

// MinFreeBytes is the room the data directory's volume must have for Ping to say
// the Store is ready. A volume closer to full than this is about to refuse writes,
// and a server that reports ready then takes deliveries it cannot keep.
const MinFreeBytes = 32 << 20

// The extended result codes of a write that failed at the file system. SQLite
// reports SQLITE_FULL only for ENOSPC; a quota (EDQUOT) or a file-size limit, which
// end the same way for the operator, come as a failed write.
const (
	sqliteIOErrWrite   = 10 | 3<<8  // SQLITE_IOERR_WRITE
	sqliteIOErrShmSize = 10 | 19<<8 // SQLITE_IOERR_SHMSIZE
)

// roomRetryWindow is how long Ping goes on reporting a Store that failed for lack
// of room. Ingest is not gated on Ping, so a later failure renews it; a Store that
// then stops failing (it was freed, or the load eased) is believed after this long.
const roomRetryWindow = 30 * time.Second

// isFull reports whether err means there was no room to write.
func isFull(err error) bool {
	var se *msqlite.Error
	if errors.As(err, &se) {
		switch c := se.Code(); {
		case c&0xff == sqliteFull, c == sqliteIOErrWrite, c == sqliteIOErrShmSize:
			return true
		}
	}
	return errors.Is(err, syscall.ENOSPC)
}

// full turns a write error into inbox.ErrStorageFull if it was one, and notes that
// the Store has run out of room, which Ping reports until a write succeeds.
func (s *Store) full(err error) error {
	if err == nil || !isFull(err) {
		return err
	}
	s.fullUntil.Store(s.now().Add(roomRetryWindow).UnixNano())
	return fmt.Errorf("%w: %w", inbox.ErrStorageFull, err)
}

// WithFreeSpace replaces the measurement of the room left on the data directory's
// volume, which Ping compares with MinFreeBytes. It exists for tests.
func WithFreeSpace(f func(dir string) (uint64, error)) Option {
	return func(c *config) { c.freeSpace = f }
}
