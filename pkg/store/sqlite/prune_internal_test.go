// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
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
	in := &inbox.Inbox{Slug: "batching", IngestKey: "batching", Name: "b", Verifier: "none"}
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
	if err := s.CreateToken(t.Context(), &inbox.Token{Name: "t", Hash: bytes.Repeat([]byte{7}, 32), InboxIDs: []inbox.ID{in.ID}}); err != nil {
		t.Fatal(err)
	}
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
		_, err := tx.ExecContext(t.Context(), `UPDATE inboxes SET disabled = 1, deleting = 1 WHERE id = ?`, int64(in.ID))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(t.Context(), `DELETE FROM token_inboxes WHERE inbox_id = ?`, int64(in.ID)); err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `DELETE FROM tokens WHERE all_inboxes = 0 AND id NOT IN (SELECT token_id FROM token_inboxes)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTokenByHash(t.Context(), bytes.Repeat([]byte{7}, 32)); err == nil {
		t.Error("the old token still resolves once the inbox is closed")
	}
	// Nothing can see it, write to it, or give it a token any more.
	if _, err := s.GetInbox(t.Context(), in.ID); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("GetInbox of an inbox being deleted: %v", err)
	}
	if _, err := s.GetInboxBySlug(t.Context(), in.Slug); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("GetInboxBySlug of an inbox being deleted: %v", err)
	}
	if _, err := s.ResolveIngestKey(t.Context(), in.IngestKey); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("ResolveIngestKey of an inbox being deleted: %v", err)
	}
	if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Verified: true}); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("Append to an inbox being deleted: %v", err)
	}
	if err := s.CreateToken(t.Context(), &inbox.Token{Name: "late", Hash: bytes.Repeat([]byte{8}, 32), InboxIDs: []inbox.ID{in.ID}}); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("CreateToken for an inbox being deleted: %v", err)
	}
	if list, _ := s.ListInboxes(t.Context(), inbox.ListParams{}); len(list) != 0 {
		t.Errorf("ListInboxes shows %d inboxes, one of them being deleted", len(list))
	}
	// The retention sweep finishes the job by itself.
	if _, err := s.Prune(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.rdb.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM inboxes`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d inboxes remain after the sweep (%v)", left, err)
	}
	// And a second call finds nothing to finish.
	if err := s.DeleteInbox(t.Context(), in.ID); !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("deleting a finished delete: %v", err)
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

// A delete that overlaps the retention sweep is still a success: whichever of the
// two removes the row last finds it already gone.
func TestADeleteThatOverlapsTheSweepSucceeds(t *testing.T) {
	for round := range 10 {
		s, in := internalStore(t)
		for range 700 {
			if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan error, 2)
		go func() { done <- s.DeleteInbox(t.Context(), in.ID) }()
		go func() { _, err := s.Prune(t.Context(), time.Now()); done <- err }()
		for range 2 {
			if err := <-done; err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if _, err := s.Prune(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		var left int
		if err := s.rdb.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM inboxes`).Scan(&left); err != nil || left != 0 {
			t.Fatalf("round %d: %d inboxes remain (%v)", round, left, err)
		}
	}
}

func TestRenewEpochs(t *testing.T) {
	s, in := internalStore(t)
	if err := s.RenewEpochs(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInbox(t.Context(), in.ID)
	if err != nil || got.Epoch == "" || got.Epoch == in.Epoch {
		t.Errorf("epoch %q after renewing, was %q (%v)", got.Epoch, in.Epoch, err)
	}
	if got.Version != in.Version+1 {
		t.Errorf("version %d, want %d: an admin holding the old copy must be made to re-read", got.Version, in.Version+1)
	}
}

// A slug that an unfinished delete still holds says so.
func TestASlugHeldByAnUnfinishedDeleteSaysWhy(t *testing.T) {
	s, in := internalStore(t)
	if err := s.writeTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE inboxes SET disabled = 1, deleting = 1 WHERE id = ?`, int64(in.ID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := s.CreateInbox(t.Context(), &inbox.Inbox{Slug: in.Slug, IngestKey: "another-key-entirely", Name: "n", Verifier: "none"})
	if !errors.Is(err, inbox.ErrConflict) || !strings.Contains(err.Error(), "being deleted") {
		t.Errorf("creating over an unfinished delete: %v", err)
	}
}

// A write that fails for lack of room is inbox.ErrStorageFull, readiness says so,
// and everything recovers when there is room again.
func TestAFullDiskIsErrStorageFull(t *testing.T) {
	s, in := internalStore(t)
	// The volume says there is room, as it does for a quota or a size limit: it is the
	// failed write that marks the Store full.
	s.freeSpace = func(string) (uint64, error) { return math.MaxUint64, nil }
	clock := time.Now()
	s.now = func() time.Time { return clock }
	if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("x"), Verified: true}); err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := s.wdb.QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// The database may grow no more: the same error SQLite gives for a full disk.
	if _, err := s.wdb.ExecContext(t.Context(), `PRAGMA max_page_count = `+strconv.Itoa(pages)); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), 1<<20)
	_, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: big, Verified: true})
	if !errors.Is(err, inbox.ErrStorageFull) {
		t.Fatalf("an append with no room: %v", err)
	}
	if err := s.Ping(t.Context()); !errors.Is(err, inbox.ErrStorageFull) {
		t.Errorf("Ping while out of room: %v", err)
	}
	// Nothing half-written was left behind.
	if st, err := s.Stats(t.Context(), in.ID); err != nil || st.MessageCount != 1 || st.LatestSeq != 1 {
		t.Errorf("stats after the failed append: %+v, %v", st, err)
	}

	// A small write that succeeds does not make it believed.
	if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("small"), Verified: true}); err == nil {
		_ = err // (it may fit in the page that was free; the point is Ping below)
	}
	if err := s.Ping(t.Context()); !errors.Is(err, inbox.ErrStorageFull) {
		t.Errorf("Ping after a small write: %v", err)
	}
	if _, err := s.wdb.ExecContext(t.Context(), `PRAGMA max_page_count = 1073741823`); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(roomRetryWindow + time.Second)
	if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: big, Verified: true}); err != nil {
		t.Fatalf("an append once there is room: %v", err)
	}
	if err := s.Ping(t.Context()); err != nil {
		t.Errorf("Ping once there is room: %v", err)
	}
}

func TestPingFailsWhenTheVolumeIsNearlyFull(t *testing.T) {
	free := uint64(1 << 30)
	s, err := Open(t.Context(), t.TempDir(), WithFreeSpace(func(string) (uint64, error) { return free, nil }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	free = MinFreeBytes / 2 // far enough below that the database's own spare pages cannot lift it
	if err := s.Ping(t.Context()); !errors.Is(err, inbox.ErrStorageFull) {
		t.Errorf("Ping on a nearly full volume: %v", err)
	}
	free = MinFreeBytes
	if err := s.Ping(t.Context()); err != nil {
		t.Errorf("Ping with exactly the minimum: %v", err)
	}
}

// Room the database has freed inside its own file is room: once pruning has
// emptied pages, a volume that reports little free space is not full.
func TestPingCountsThePagesTheDatabaseCanReuse(t *testing.T) {
	free := uint64(MinFreeBytes - 1<<20)
	s, err := Open(t.Context(), t.TempDir(), WithFreeSpace(func(string) (uint64, error) { return free, nil }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	in := &inbox.Inbox{Slug: "spare", IngestKey: "spare", Name: "s", Verifier: "none"}
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: make([]byte, 1<<20), Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.wdb.ExecContext(t.Context(), `DELETE FROM messages`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(t.Context()); err != nil {
		t.Errorf("Ping with several MiB of reusable pages: %v", err)
	}
	if got := s.reusableBytes(t.Context()); got < 2<<20 {
		t.Errorf("reusable bytes: %d", got)
	}
}
