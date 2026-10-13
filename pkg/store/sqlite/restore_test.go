// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// A database file with nothing in it is what a restore leaves when the copy did not
// finish. It must not be taken for a new installation: the server would start, look
// healthy, and have none of the data.
func TestAnEmptyDatabaseFileIsRefusedNotTakenForANewOne(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, sqlite.DBFileName)
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, opts := range [][]sqlite.Option{nil, {sqlite.WithExclusiveLock()}} {
		s, err := sqlite.Open(t.Context(), dir, opts...)
		if err == nil {
			_ = s.Close()
			t.Fatal("an empty hookinlet.db was opened as a new database")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("the error does not say the file is empty: %v", err)
		}
	}
	if fi, err := os.Stat(db); err != nil || fi.Size() != 0 {
		t.Errorf("the empty file was changed: %v, %v", fi, err)
	}
	if _, err := os.Stat(db + "-wal"); err == nil {
		t.Error("a write-ahead log was started beside the empty file")
	}
}

// A first start that fails must not leave behind the empty file that the test above
// refuses, or the next try would be turned away for something the operator never did.
func TestAFirstOpenThatFailsLeavesNoEmptyDatabaseFile(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if s, err := sqlite.Open(ctx, dir); err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded with a cancelled context")
	}
	s, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("the next Open was refused: %v", err)
	}
	_ = s.Close()
}
