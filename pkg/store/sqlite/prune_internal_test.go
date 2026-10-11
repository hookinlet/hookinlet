// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func internalStore(t *testing.T) (*Store, *inbox.Inbox) {
	t.Helper()
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	in := &inbox.Inbox{Slug: "batching", Name: "b", Verifier: "none", AgentTokenHash: []byte("hash-batching-0123")}
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	return s, in
}

func batch(t *testing.T, s *Store, in *inbox.Inbox, bound int64) int64 {
	t.Helper()
	var n int64
	err := s.writeTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		n, err = deleteBatch(t.Context(), tx, in.ID, bound)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A batch is bounded by rows for small messages and by bytes for large ones,
// because what an append waits for is how long the batch takes.
func TestABatchIsBoundedByRowsAndByBytes(t *testing.T) {
	s, in := internalStore(t)
	for range 300 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	if n := batch(t, s, in, 300); n != pruneBatchRows {
		t.Errorf("a batch of small messages deleted %d, want %d", n, pruneBatchRows)
	}
	if n := batch(t, s, in, 300); n != 50 {
		t.Errorf("the remainder was %d, want 50", n)
	}
	if n := batch(t, s, in, 300); n != 0 {
		t.Errorf("an empty batch deleted %d", n)
	}

	big := bytes.Repeat([]byte("b"), 1<<20)
	for range 10 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: big, Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	if n := batch(t, s, in, math.MaxInt64); n != pruneBatchBytes/(1<<20) {
		t.Errorf("a batch of 1 MiB messages deleted %d, want %d", n, pruneBatchBytes/(1<<20))
	}

	// A single message bigger than the byte bound still goes, alone.
	s2, in2 := internalStore(t)
	in2.MaxBodyBytes = 4 << 20
	if err := s2.UpdateInbox(t.Context(), in2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.Append(t.Context(), in2.ID, &inbox.NewMessage{Method: "POST", Body: bytes.Repeat([]byte("z"), 3<<20), Verified: true}); err != nil {
		t.Fatal(err)
	}
	if n := batch(t, s2, in2, 1); n != 1 {
		t.Errorf("an oversized message deleted %d, want 1", n)
	}
}

// retention_days is validated, but a database written by something else could
// hold a value so large that the duration overflows into the future and every
// message looks expired.
func TestAHugeRetentionInTheDatabaseDeletesNothing(t *testing.T) {
	s, in := internalStore(t)
	for range 10 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, days := range []int64{106751, 106752, 1_000_000, math.MaxInt32} {
		if _, err := s.wdb.ExecContext(t.Context(), `UPDATE inboxes SET retention_days = ? WHERE id = ?`, days, int64(in.ID)); err != nil {
			t.Fatal(err)
		}
		if n, err := s.Prune(t.Context(), time.Now()); err != nil || n != 0 {
			t.Errorf("retention_days = %d deleted %d messages (%v)", days, n, err)
		}
	}
}

func TestAnInboxIsClosedToAgentsAndIngestBeforeItsMessagesGo(t *testing.T) {
	s, in := internalStore(t)
	for range 600 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	// Interrupt the delete after its first step by cancelling when the first
	// batch is about to run: the inbox must already be closed.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.DeleteInbox(ctx, in.ID); err == nil {
		t.Fatal("a cancelled delete succeeded")
	}
	// (Cancelled before it began, so nothing changed.)
	got, err := s.GetInbox(t.Context(), in.ID)
	if err != nil || got.Disabled {
		t.Fatalf("a delete that never began changed the inbox: %+v %v", got, err)
	}

	// Run the first step alone, as an interrupted delete would leave things.
	if err := s.writeTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE inboxes SET disabled = 1, agent_token_hash = randomblob(32) WHERE id = ?`, int64(in.ID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInboxByTokenHash(t.Context(), in.AgentTokenHash); err == nil {
		t.Error("the old token still resolves once the inbox is closed")
	}
	// Deleting again finishes the job.
	if err := s.DeleteInbox(t.Context(), in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInbox(t.Context(), in.ID); err == nil {
		t.Error("the inbox is still there")
	}
	var n int
	if err := s.rdb.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE inbox_id = ?`, int64(in.ID)).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d messages remain (%v)", n, err)
	}
	if err := s.DeleteInbox(t.Context(), in.ID); err == nil {
		t.Error("deleting a deleted inbox succeeded")
	}
}
