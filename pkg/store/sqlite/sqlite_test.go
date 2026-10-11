// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

func TestStoreSuite(t *testing.T) {
	inboxtest.StoreSuite(t, func(t *testing.T, now func() time.Time) inbox.Store {
		s, err := sqlite.Open(t.Context(), t.TempDir(), sqlite.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func openAt(t *testing.T, dir string, opts ...sqlite.Option) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(t.Context(), dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newInbox(slug string) *inbox.Inbox {
	sum := sha256.Sum256([]byte(slug))
	return &inbox.Inbox{Slug: slug, IngestKey: slug, Name: slug, Verifier: "none", AgentTokenHash: sum[:]}
}

func TestDataSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	s := openAt(t, dir)
	in := newInbox("persistent")
	if err := s.CreateInbox(ctx, in); err != nil {
		t.Fatal(err)
	}
	body := []byte{0xff, 0xfe, 0x00, 'x'}
	for range 3 {
		if _, _, err := s.Append(ctx, in.ID, &inbox.NewMessage{Method: "POST", Body: body, Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	s = openAt(t, dir)
	t.Cleanup(func() { _ = s.Close() })
	if len(s.AppliedMigrations()) != 0 {
		t.Errorf("reopening an up-to-date database applied migrations: %v", s.AppliedMigrations())
	}
	got, err := s.GetInboxBySlug(ctx, "persistent")
	if err != nil || got.LastSeq != 3 {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
	m, err := s.Get(ctx, in.ID, 2)
	if err != nil || !bytes.Equal(m.Body, body) {
		t.Fatalf("message after reopen: %+v, %v", m, err)
	}
	next, _, err := s.Append(ctx, in.ID, &inbox.NewMessage{Method: "POST", Verified: true})
	if err != nil || next.Seq != 4 {
		t.Errorf("sequence did not continue across a restart: seq %d, %v", next.Seq, err)
	}
}

func TestOpenAppliesMigrationsOnce(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	t.Cleanup(func() { _ = s.Close() })
	files, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	got := s.AppliedMigrations()
	if len(got) != len(files) {
		t.Fatalf("fresh database applied %v, want 1 to %d", got, len(files))
	}
	for i, v := range got {
		if v != i+1 {
			t.Errorf("fresh database applied %v, want 1 to %d", got, len(files))
		}
	}
	if v, err := s.SchemaVersion(t.Context()); err != nil || v != len(files) {
		t.Errorf("SchemaVersion = %d, %v", v, err)
	}
}

func TestRefusesDatabaseFromANewerVersion(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO schema_migrations (version, applied_at) VALUES (999, 'future')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	_, err = sqlite.Open(t.Context(), dir)
	if err == nil || !strings.Contains(err.Error(), "newer than this build supports") {
		t.Fatalf("want a clear error about the newer schema, got %v", err)
	}
}

// Migrations are append-only (guardrail 20). This pins the hash of every
// shipped migration: editing one fails here. To change the schema, add a new
// numbered file and append its hash.
func TestShippedMigrationsAreUnchanged(t *testing.T) {
	pinned := map[string]string{
		"0001_init.sql":                      "5e5fd5ab3d11008906bf1f7256d06a5fa733d7f40b3248e76b8ba23bf91dc6c5",
		"0002_delivery_index.sql":            "ae5b3395b0f4df9faada44c0332c2a6aa52f1137a477150ceb0d95c27d406081",
		"0003_ingest_key_and_dedupe_key.sql": "ab08447e727ec057144111db17dd79ae940f8c9f75fa839e82df8d6646118fa1",
		"0004_random_ingest_keys.sql":        "c7a59a986049078495f68dd7ff74f3a855993ab0ca8bf42cb08fdd39a8f9c766",
	}
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		want, known := pinned[e.Name()]
		if !known {
			t.Errorf("migration %s has no pinned hash; add %q", e.Name(), hex.EncodeToString(sum[:]))
			continue
		}
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("migration %s changed: sha256 %s, pinned %s. Shipped migrations must not be edited.", e.Name(), got, want)
		}
	}
	if len(entries) != len(pinned) {
		t.Errorf("found %d migration files, %d pinned", len(entries), len(pinned))
	}
}

func TestConnectionSettings(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	t.Cleanup(func() { _ = s.Close() })

	// Open the same file the way the store does and read the persistent setting;
	// per-connection settings are verified through observable behavior below.
	raw, err := sql.Open("sqlite", filepath.Join(dir, sqlite.DBFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var mode string
	if err := raw.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}

	// foreign_keys is on for the store's connections: deleting an inbox removes
	// its messages.
	ctx := t.Context()
	in := newInbox("cascade")
	if err := s.CreateInbox(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(ctx, in.ID, &inbox.NewMessage{Method: "POST", Verified: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteInbox(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := raw.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d orphaned messages after deleting the inbox, %v", n, err)
	}
}

// Reads go through their own pool and must keep working, and never fail with a
// busy database, while the writer is busy.
func TestReadersAndWriterOverlap(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir, sqlite.WithReaders(2))
	t.Cleanup(func() { _ = s.Close() })
	in := newInbox("overlap")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 8 {
		wg.Go(func() {
			for range 25 {
				if _, err := s.GetInbox(t.Context(), in.ID); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Verified: true}); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Guardrail 17 and the Phase 1 gate: redacted headers are never persisted. This
// checks the bytes on disk, not just what the API returns.
func TestRedactedHeadersNeverReachDisk(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	s := openAt(t, dir)
	in := newInbox("disk-redaction")
	if err := s.CreateInbox(ctx, in); err != nil {
		t.Fatal(err)
	}
	secrets := []string{"Bearer disk-secret-token", "session=disk-cookie-secret", "Basic ZGlzay1wcm94eQ=="}
	if _, _, err := s.Append(ctx, in.ID, &inbox.NewMessage{
		Method:   "POST",
		Verified: true,
		Headers: http.Header{
			"Authorization":       {secrets[0]},
			"Cookie":              {secrets[1]},
			"Proxy-Authorization": {secrets[2]},
			"X-Visible":           {"fine"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // checkpoints the WAL into the main file
		t.Fatal(err)
	}

	var all []byte
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // files in a temp directory this test created
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	for _, secret := range secrets {
		if bytes.Contains(all, []byte(secret)) {
			t.Errorf("secret %q found in the database files", secret)
		}
	}
	if !bytes.Contains(all, []byte("X-Visible")) {
		t.Error("sanity check failed: the ordinary header is missing from the files")
	}
}

// The database holds message bodies and sealed secrets, so no other user may
// read it, including the WAL and shared-memory files SQLite creates beside it.
func TestDatabaseFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	in := newInbox("private")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Verified: true}); err != nil {
		t.Fatal(err)
	}
	// While open, the WAL files exist too.
	files, _ := filepath.Glob(filepath.Join(dir, sqlite.DBFileName+"*"))
	if len(files) == 0 {
		t.Fatal("no database files found")
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s has mode %o; group and others must have no access", filepath.Base(f), mode)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenFailsClearlyOnAnUnusableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(t.Context(), filepath.Join(file, "sub")); err == nil {
		t.Error("opening a directory underneath a regular file succeeded")
	}
	if _, err := sqlite.Open(t.Context(), t.TempDir(), sqlite.WithReaders(0)); err == nil {
		t.Error("zero readers accepted")
	}
}

func TestStartupIsFast(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	in := newInbox("warm")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: bytes.Repeat([]byte("x"), 1024), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	s = openAt(t, dir)
	elapsed := time.Since(start)
	t.Cleanup(func() { _ = s.Close() })
	// The plan budgets 500 ms for a cold start; reopening an existing database
	// is a small fraction of that. The bound is loose so slow CI hosts and the
	// race detector do not make it flaky.
	if elapsed > 500*time.Millisecond {
		t.Errorf("reopening an existing database took %v", elapsed)
	}
	t.Logf("reopen took %v", elapsed)
}

func TestPingFailsAfterClose(t *testing.T) {
	s := openAt(t, t.TempDir())
	if err := s.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Error("Ping succeeded on a closed store")
	}
}

func TestManyInboxesManyReaders(t *testing.T) {
	s := openAt(t, t.TempDir())
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	var ids []inbox.ID
	for i := range 20 {
		in := newInbox(fmt.Sprintf("inbox-%02d", i))
		if err := s.CreateInbox(ctx, in); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, in.ID)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Go(func() {
			for range 20 {
				if _, _, err := s.Append(ctx, id, &inbox.NewMessage{Method: "POST", Verified: true}); err != nil {
					errs <- err
					return
				}
			}
			page, err := s.Query(ctx, id, inbox.Query{})
			if err != nil {
				errs <- fmt.Errorf("inbox %d: %w", id, err)
				return
			}
			if len(page.Messages) != 20 {
				errs <- fmt.Errorf("inbox %d: %d messages, want 20", id, len(page.Messages))
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestBackupperSuite(t *testing.T) {
	inboxtest.BackupperSuite(t,
		func(t *testing.T) inbox.Store { return openAt(t, t.TempDir()) },
		func(t *testing.T, snapshot []byte) inbox.Store {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, sqlite.DBFileName), snapshot, 0o600); err != nil {
				t.Fatal(err)
			}
			return openAt(t, dir)
		})
}

func TestBackupPassesAnIntegrityCheckAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	defer func() { _ = s.Close() }()
	in := newInbox("integrity")
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte(strings.Repeat("x", 1000)), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	rc, size, err := s.Backup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "copy.db")
	data, err := io.ReadAll(rc)
	if err != nil || int64(len(data)) != size {
		t.Fatalf("read %d of %d bytes: %v", len(data), size, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snap, data, 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+snap+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		t.Errorf("integrity_check = %q, %v", result, err)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 200 {
		t.Errorf("the copy holds %d messages, %v", n, err)
	}

	left, _ := filepath.Glob(filepath.Join(dir, "backup-*"))
	if len(left) != 0 {
		t.Errorf("closing the snapshot left %v behind", left)
	}
}

func TestOpenRemovesOnlyOldLeftoverSnapshots(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "backup-old.tmp")
	fresh := filepath.Join(dir, "backup-fresh.tmp")
	other := filepath.Join(dir, "notes.tmp")
	for _, f := range []string{old, fresh, other} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, dir)
	_ = s.Close()
	for f, wantGone := range map[string]bool{old: true, fresh: false, other: false} {
		_, err := os.Stat(f)
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s: removed = %v, want %v", filepath.Base(f), gone, wantGone)
		}
	}
}

func TestBackupHonorsACancelledContext(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.Backup(ctx); err == nil {
		t.Error("a cancelled backup succeeded")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backup-*")); len(left) != 0 {
		t.Errorf("a cancelled backup left %v behind", left)
	}
}
