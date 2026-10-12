// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package sqlite is the default inbox.Store: a single-node store on SQLite.
//
// Single-node assumption: exactly one process may open a data directory. All
// writes go through one connection (guardrail 19), and reads use a separate
// pool of read-only connections so they never wait for the writer. The WAL
// journal gives readers a consistent snapshot while a write is in progress.
//
// Stability: this package is pre-1.0. Its API can still change, and every change is
// listed in CHANGELOG.md; after v1.0.0, semantic versioning applies.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// DBFileName is the database file inside the data directory.
const DBFileName = "hookinlet.db"

const (
	defaultReaders = 4
	// busyTimeout is only a safety net against a second process touching the
	// file; this process serializes its own writes.
	busyTimeout = 5000 // ms
	// pruneBatchRows bounds how many rows one Prune transaction deletes, so
	// appends can interleave with a large prune. Appends wait for the single
	// writer, so this is also the longest one can be made to wait. Measured while
	// a million rows were pruned and four senders appended (see
	// TestIngestLatencyWhileAMillionRowsArePruned), the append p99 was 279 ms at
	// 5000 rows, 48 ms at 1000, 32 ms at 500, 16 ms at 250 and 10 ms at 100, for
	// a prune that took 9, 9, 10, 11 and 14 seconds.
	pruneBatchRows = 250
	// pruneBatchBytes also bounds a batch by the size of the bodies in it, since
	// a batch of large messages takes far longer than 250 small ones.
	pruneBatchBytes = 2 << 20
)

// Store implements inbox.Store on SQLite.
type Store struct {
	path      string  // the database file
	wdb       *sql.DB // exactly one connection
	rdb       *sql.DB // read-only pool
	now       func() time.Time
	applied   []int
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error

	log       *slog.Logger
	lock      *os.File     // held for the life of a Store opened with WithExclusiveLock
	clockWarn atomic.Int64 // when a stepped-back clock was last logged, in unix seconds
}

var _ inbox.Store = (*Store)(nil)

// Option configures Open.
type Option func(*config)

type config struct {
	now     func() time.Time
	readers int
	log     *slog.Logger
	lock    bool
}

// WithClock sets the clock used for every timestamp the Store assigns. It
// exists so tests can control time; the default is time.Now.
func WithClock(now func() time.Time) Option { return func(c *config) { c.now = now } }

// LockFileName is the file in the data directory that WithExclusiveLock locks.
const LockFileName = "hookinlet.lock"

// WithLogger sets where the Store reports things an operator should know, such as
// a clock that stepped back. The default discards them.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.log = l } }

// WithExclusiveLock makes Open take a lock on the data directory that no other
// exclusive Open can share, and fails with *LockedError if another holds it. A
// server uses it, so that a second server started on the same directory stops at
// once and not after it has done harm; so do the commands that must not run beside
// one (rekey, renew-epochs). Stores opened without it are not blocked by it, which
// is what lets the command line work in the data directory of a running server. The
// lock goes when the Store is closed or the process ends.
func WithExclusiveLock() Option { return func(c *config) { c.lock = true } }

// LockedError means the data directory is locked by another process.
type LockedError struct {
	Path   string
	Holder string // the pid it wrote, if it could be read
}

func (e *LockedError) Error() string {
	who := ""
	if h := strings.TrimSpace(e.Holder); h != "" {
		who = " (process " + h + ")"
	}
	return "sqlite: another hookinlet process" + who + " holds the lock on this data directory (" + e.Path + "): a second server cannot share it, and the commands that need the server stopped cannot run beside one"
}

// WithReaders sets the size of the read-connection pool. The default is 4.
func WithReaders(n int) Option { return func(c *config) { c.readers = n } }

// Open opens (creating if necessary) the database in dir and applies any
// pending migrations. It makes no network calls. It fails if the database was
// written by a newer version of hookinlet.
func Open(ctx context.Context, dir string, opts ...Option) (_ *Store, err error) {
	cfg := config{now: time.Now, readers: defaultReaders}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.readers < 1 {
		return nil, errors.New("sqlite: readers must be at least 1")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sqlite: create data directory: %w", err)
	}
	abs, err := filepath.Abs(filepath.Join(dir, DBFileName))
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}

	var lock *os.File
	if cfg.lock {
		if lock, err = lockFile(filepath.Join(filepath.Dir(abs), LockFileName)); err != nil {
			return nil, err
		}
	}
	defer func() {
		if err != nil && lock != nil {
			unlockFile(lock)
		}
	}()

	// Create the database file ourselves so it is private to the owner. SQLite
	// gives the -wal and -shm files the same permissions as the main file.
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the path is the configured data directory plus a fixed name
	if err != nil {
		return nil, fmt.Errorf("sqlite: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: create database file: %w", err)
	}

	// The writer is opened first: it creates the file and switches it to WAL.
	// _txlock=immediate makes every write transaction take the write lock when
	// it begins, so a transaction never fails halfway with SQLITE_BUSY.
	wdb, err := openDB(abs, cfg.readers, false)
	if err != nil {
		return nil, err
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)

	removeStaleBackups(filepath.Dir(abs), time.Now())
	if cfg.log == nil {
		cfg.log = slog.New(slog.DiscardHandler)
	}
	s := &Store{path: abs, wdb: wdb, now: cfg.now, log: cfg.log, lock: lock}
	if err := wdb.PingContext(ctx); err != nil {
		_ = wdb.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", abs, err)
	}
	if s.applied, err = migrate(ctx, wdb, abs); err != nil {
		_ = wdb.Close()
		return nil, err
	}

	rdb, err := openDB(abs, cfg.readers, true)
	if err != nil {
		_ = wdb.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(cfg.readers)
	rdb.SetMaxIdleConns(cfg.readers)
	rdb.SetConnMaxLifetime(0)
	s.rdb = rdb
	if err := rdb.PingContext(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("sqlite: open read pool: %w", err)
	}
	return s, nil
}

func openDB(path string, _ int, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeout),
		"journal_mode(WAL)",
		"foreign_keys(1)",
		// FULL syncs the WAL on every commit, so a message acknowledged with a 2xx
		// survives power loss and not only a crash of this process. NORMAL would
		// leave the latest commits unsynced.
		"synchronous(FULL)",
	} {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	return db, nil
}

// AppliedMigrations returns the versions applied by the Open that created s,
// in order. It is empty when the database was already up to date.
func (s *Store) AppliedMigrations() []int { return append([]int(nil), s.applied...) }

// Close checkpoints the WAL and releases the database. It is safe to call more
// than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		var errs []error
		if s.rdb != nil {
			errs = append(errs, s.rdb.Close())
		}
		if s.wdb != nil {
			// Fold the WAL back into the main file so a stopped data directory is
			// a single self-contained file, safe to copy.
			_, _ = s.wdb.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
			errs = append(errs, s.wdb.Close())
		}
		if s.lock != nil {
			unlockFile(s.lock) // last: nothing else may open the directory until it is closed
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// Ping implements inbox.Store.
func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.rdb.PingContext(ctx); err != nil {
		return s.ctxErr(ctx, fmt.Errorf("sqlite: ping reader: %w", err))
	}
	if err := s.wdb.PingContext(ctx); err != nil {
		return s.ctxErr(ctx, fmt.Errorf("sqlite: ping writer: %w", err))
	}
	return nil
}

// ---- transactions and errors ----

// ctxErr reports the context's error in place of err when the context was
// cancelled, so that cancellation always surfaces as context.Canceled or
// context.DeadlineExceeded no matter where the driver noticed it.
func (s *Store) ctxErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("sqlite: %w", cerr)
	}
	return err
}

// writeTx runs fn in a write transaction on the single writer connection.
func (s *Store) writeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return s.ctxErr(ctx, fmt.Errorf("sqlite: begin: %w", err))
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return s.ctxErr(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return s.ctxErr(ctx, fmt.Errorf("sqlite: commit: %w", err))
	}
	return nil
}

// readTx runs fn in a read transaction, which sees one consistent snapshot.
func (s *Store) readTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.rdb.BeginTx(ctx, nil)
	if err != nil {
		return s.ctxErr(ctx, fmt.Errorf("sqlite: begin read: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return s.ctxErr(ctx, err)
	}
	return nil
}

// ---- time ----

// timeLayout is fixed-width so that lexicographic order equals chronological
// order, which the received_at indexes and range comparisons rely on.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func formatTime(t time.Time) string { return inbox.NormalizeTime(t).Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.ParseInLocation(timeLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: corrupt timestamp %q: %w", s, err)
	}
	return t, nil
}

// ceilMicro rounds t up to the next TimePrecision boundary. Stored timestamps
// are whole microseconds, so for a stored r: r >= t is r >= ceilMicro(t), and
// r < t is r < ceilMicro(t).
func ceilMicro(t time.Time) time.Time {
	if f := t.UTC().Truncate(inbox.TimePrecision); !f.Equal(t) {
		return f.Add(inbox.TimePrecision)
	}
	return t.UTC()
}
