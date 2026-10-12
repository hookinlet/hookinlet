// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the embedded migrations, ordered by version. File names
// are NNNN_description.sql; versions must be unique and start at 1.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlite: read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, convErr := strconv.Atoi(num)
		if !ok || convErr != nil || v < 1 {
			return nil, fmt.Errorf("sqlite: migration %q must be named NNNN_description.sql", e.Name())
		}
		body, err := fs.ReadFile(migrationFiles, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("sqlite: read migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("sqlite: migrations must be numbered 1..N without gaps; found %s at position %d", m.name, i+1)
		}
	}
	return out, nil
}

// migrate applies pending migrations, each in its own transaction, and returns
// the versions it applied. It refuses to run against a database that has
// migrations this binary does not know, since that means the data was written
// by a newer version.
func migrate(ctx context.Context, db *sql.DB, dbPath string, mayMigrate bool, log *slog.Logger) ([]int, error) {
	migs, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	// The bookkeeping table is created here rather than in a migration because
	// migrations need it to exist before the first one can be recorded.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return nil, fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return nil, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if current > len(migs) {
		return nil, fmt.Errorf("sqlite: database schema version %d is newer than this build supports (%d); upgrade hookinlet", current, len(migs))
	}

	// Migrating changes the schema under any process that has the database open, so
	// it is left to the process that holds the lock on the data directory: a command
	// that merely looks at the data, run beside an older server, must not do it.
	if current > 0 && current < len(migs) && !mayMigrate {
		return nil, &NeedsMigrationError{From: current, To: len(migs)}
	}

	// Migrations only move forward, so before changing a database that has data in
	// it, keep a copy of it as it was: if an upgrade turns out badly, that copy and
	// the previous release are the way back. A database that has never been migrated
	// (a new one) has nothing to keep. An upgrade that was cut short leaves a marker
	// behind, and the next start keeps the copy it already has: the database is by
	// then partly upgraded, and a copy of that is no way back.
	marker := filepath.Join(filepath.Dir(dbPath), migrationMarker)
	if current > 0 && current < len(migs) {
		_, markerErr := os.Stat(marker)
		haveCopy, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), MigrationSnapshotPrefix+"*.bak"))
		// The marker records the schema version and a fingerprint of the data when the
		// copy was made. An upgrade that was killed before its first migration committed
		// finds both unchanged, and keeps the copy. A rolled-back one has had the copy put
		// back and may have taken deliveries since, so the copy is no longer a way back to
		// what is there now: take a new one.
		stamp, err := fingerprint(ctx, db)
		if err != nil {
			return nil, err
		}
		started, _ := os.ReadFile(marker) //nolint:gosec // beside the database, in the data directory
		same := strings.TrimSpace(string(started)) == strconv.Itoa(current)+" "+stamp
		fresh := strings.TrimSpace(string(started)) == strconv.Itoa(current) || strings.HasPrefix(string(started), strconv.Itoa(current)+" ")
		if markerErr != nil || len(haveCopy) == 0 || (fresh && !same) {
			if err := snapshotBeforeMigrating(ctx, db, dbPath, current); err != nil {
				return nil, err
			}
			log.Info("kept a copy of the database before migrating it", "from_version", current, "to_version", len(migs), "dir", filepath.Dir(dbPath))
			if err := os.WriteFile(marker, []byte(strconv.Itoa(current)+" "+stamp+"\n"), 0o600); err != nil {
				return nil, fmt.Errorf("sqlite: %w", err)
			}
		}
	}

	var applied []int
	for _, m := range migs[current:] {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return applied, fmt.Errorf("sqlite: begin migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("sqlite: apply migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UTC().Format(timeLayout)); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("sqlite: record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return applied, fmt.Errorf("sqlite: commit migration %s: %w", m.name, err)
		}
		applied = append(applied, m.version)
		log.Info("applied migration", "name", m.name)
	}
	if len(applied) > 0 {
		_ = os.Remove(marker) // the upgrade finished
	}
	return applied, nil
}

// fingerprint summarises what a delivery or an inbox change would alter, cheaply enough
// to take on a large database.
func fingerprint(ctx context.Context, db *sql.DB) (string, error) {
	var s string
	err := db.QueryRowContext(ctx, `SELECT (SELECT COALESCE(MAX(id),0) FROM messages) || '/' ||
		(SELECT COUNT(*) FROM messages) || '/' || (SELECT COUNT(*) FROM inboxes)`).Scan(&s)
	if err != nil {
		return "", fmt.Errorf("sqlite: fingerprint the database: %w", err)
	}
	return s, nil
}

// NeedsMigrationError means the database is at an older schema than this build and
// the Store was opened without the data-directory lock that applying migrations needs.
type NeedsMigrationError struct{ From, To int }

func (e *NeedsMigrationError) Error() string {
	return fmt.Sprintf("sqlite: the database is at schema version %d and this release needs %d; stop the server and run `hookinlet migrate` to upgrade it", e.From, e.To)
}

// SchemaVersion returns the newest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, s.ctxErr(ctx, fmt.Errorf("sqlite: read schema version: %w", err))
	}
	return v, nil
}

// MigrationSnapshotPrefix begins the name of the copy of the database that is made
// before a migration: hookinlet.db.before-migration-N.bak, where N is the schema
// version it was at. Only the latest is kept; delete it once the upgrade is trusted.
const MigrationSnapshotPrefix = DBFileName + ".before-migration-"

// migrationMarker exists while an upgrade is under way (and after one that did not
// finish), and holds the schema version it started from.
const migrationMarker = DBFileName + ".migrating"

// snapshotBeforeMigrating writes the copy, and only once it is complete does it
// replace the copy of any earlier upgrade. A copy that fails to be made costs the
// operator nothing they had.
func snapshotBeforeMigrating(ctx context.Context, db *sql.DB, dbPath string, from int) error {
	dir := filepath.Dir(dbPath)
	name := filepath.Join(dir, MigrationSnapshotPrefix+strconv.Itoa(from)+".bak")
	partial := name + ".partial"
	_ = os.Remove(partial)
	// VACUUM INTO accepts an empty file. Making it ourselves gives it its mode from
	// the start; it is not created wider and narrowed afterwards.
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // beside the database, in the data directory
	if err != nil {
		return fmt.Errorf("sqlite: could not keep a copy of the database before migrating it: %w", err)
	}
	_ = f.Close()
	// One consistent copy, in one read transaction.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, partial); err != nil {
		_ = os.Remove(partial)
		_ = os.Remove(partial + "-journal")
		return fmt.Errorf("sqlite: could not keep a copy of the database before migrating it (is there room for a second copy in %s?); nothing was changed: %w", dir, err)
	}
	if err := os.Rename(partial, name); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("sqlite: pre-migration copy: %w", err)
	}
	others, _ := filepath.Glob(filepath.Join(dir, MigrationSnapshotPrefix+"*.bak"))
	for _, o := range others {
		if o != name {
			_ = os.Remove(o) // the copy of an earlier, finished upgrade
		}
	}
	return nil
}
