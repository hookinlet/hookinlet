// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
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
func migrate(ctx context.Context, db *sql.DB) ([]int, error) {
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
	}
	return applied, nil
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
