// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

//go:build unix

package sqlite_test

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

func TestASecondExclusiveOpenIsRefusedUntilTheFirstCloses(t *testing.T) {
	dir := t.TempDir()
	first, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatal(err)
	}
	_, err = sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	var locked *sqlite.LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("a second exclusive open: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("the error does not name the holder: %v", err)
	}
	// The command line, which does not ask for the lock, still works beside a server.
	plain, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("a plain open beside a server: %v", err)
	}
	_ = plain.Close()

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatalf("after the first closed: %v", err)
	}
	_ = again.Close()
}

// A failed open does not keep the lock.
func TestAnOpenThatFailsReleasesTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/"+sqlite.DBFileName, []byte("this is not a database, and is long enough to be read as one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock()); err == nil {
		t.Skip("a garbage file was accepted as a database")
	}
	if err := os.Remove(dir + "/" + sqlite.DBFileName); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(t.Context(), dir, sqlite.WithExclusiveLock())
	if err != nil {
		t.Fatalf("the lock outlived the failed open: %v", err)
	}
	_ = s.Close()
}
