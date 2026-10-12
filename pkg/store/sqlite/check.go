// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"context"
	"fmt"
	"strings"
)

// maxProblems bounds what Check reports, so a badly damaged database does not
// produce a report as large as itself.
const maxProblems = 50

// Check asks SQLite to verify the database file: that its pages, indexes and
// constraints are consistent. It returns the problems found, none if the database
// is sound. quick skips the comparison of indexes with their tables, which is
// most of the work on a large database. Either way it reads the whole file, so on
// a large database it takes seconds to minutes; it can run beside a server.
func (s *Store) Check(ctx context.Context, quick bool) ([]string, error) {
	pragma := "integrity_check"
	if quick {
		pragma = "quick_check"
	}
	rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf("PRAGMA %s(%d)", pragma, maxProblems))
	if err != nil {
		return damage(ctx, s, pragma, err)
	}
	defer func() { _ = rows.Close() }()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("sqlite: %s: %w", pragma, err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return damage(ctx, s, pragma, err)
	}
	return problems, nil
}

// damage turns the error SQLite gives when the file is too damaged to check into the
// finding it is; any other error is a failure to check.
func damage(ctx context.Context, s *Store, pragma string, err error) ([]string, error) {
	msg := err.Error()
	if ctx.Err() == nil && (strings.Contains(msg, "malformed") || strings.Contains(msg, "SQLITE_CORRUPT") || strings.Contains(msg, "not a database")) {
		return []string{msg}, nil
	}
	return nil, s.ctxErr(ctx, fmt.Errorf("sqlite: %s: %w", pragma, err))
}
