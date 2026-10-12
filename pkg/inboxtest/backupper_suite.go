// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"fmt"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// BackupperSuite runs the inbox.Backupper conformance tests.
//
// newStore returns a new, empty Store that implements inbox.Backupper, and
// restore opens a Store from the bytes of a snapshot. The suite closes every
// Store it is given, so neither function should register its own Close.
func BackupperSuite(t *testing.T, newStore func(t *testing.T) inbox.Store, restore func(t *testing.T, snapshot []byte) inbox.Store) {
	t.Helper()

	take := func(t *testing.T, s inbox.Store) []byte {
		t.Helper()
		b, ok := s.(inbox.Backupper)
		if !ok {
			t.Fatal("the Store does not implement inbox.Backupper")
		}
		rc, size, err := b.Backup(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("closing the snapshot: %v", err)
		}
		if size < -1 {
			t.Errorf("size = %d: it is the length, or -1 for a snapshot that streams", size)
		}
		if size >= 0 && int64(len(data)) != size {
			t.Errorf("the reader yielded %d bytes but size said %d", len(data), size)
		}
		return data
	}
	all := func(t *testing.T, s inbox.Store, in *inbox.Inbox) []*inbox.Message {
		t.Helper()
		after := int64(0)
		var out []*inbox.Message
		for {
			p, err := s.Query(t.Context(), in.ID, inbox.Query{After: &after, Limit: inbox.MaxQueryLimit})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range p.Messages {
				full, err := s.Get(t.Context(), in.ID, m.Seq)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, full)
			}
			if len(p.Messages) < inbox.MaxQueryLimit {
				return out
			}
			after = p.Messages[len(p.Messages)-1].Seq
		}
	}

	t.Run("ASnapshotRestoresEverything", func(t *testing.T) {
		s := newStore(t)
		defer func() { _ = s.Close() }()
		ctx := t.Context()

		a := &inbox.Inbox{Slug: "backup-a", IngestKey: "backup-a", Name: "A", Verifier: "hmac", VerifierConfig: []byte(`{"header":"X-Sig"}`),
			VerifierSecret: []byte("sealed-bytes-a"), OnVerifyFail: inbox.OnVerifyFailStore,
			RetentionDays: 7, RetentionMaxCount: 1000, EventTypePointer: "/type", MaxBodyBytes: 4096}
		b := &inbox.Inbox{Slug: "backup-b", IngestKey: "backup-b", Name: "B", Verifier: "none", Disabled: true}
		for _, in := range []*inbox.Inbox{a, b} {
			if err := s.CreateInbox(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 25; i++ {
			body := []byte(fmt.Sprintf(`{"n":%d}`, i))
			if i%5 == 0 {
				body = []byte{0xff, 0xfe, byte(i)} // not text
			}
			if _, _, err := s.Append(ctx, a.ID, &inbox.NewMessage{Method: "POST", PathSuffix: "/x", Query: "q=1", ContentType: "application/json",
				Headers: map[string][]string{"X-A": {"1", "2"}}, Body: body, Verified: i%2 == 0, DeliveryID: fmt.Sprintf("d%d", i), EventType: "t"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := s.Append(ctx, b.ID, &inbox.NewMessage{Method: "PUT", Body: []byte("b"), Verified: true}); err != nil {
			t.Fatal(err)
		}

		r := restore(t, take(t, s))
		defer func() { _ = r.Close() }()
		for _, created := range []*inbox.Inbox{a, b} {
			want, err := s.GetInboxBySlug(ctx, created.Slug) // as it is now, with its LastSeq
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.GetInboxBySlug(ctx, want.Slug)
			if err != nil {
				t.Fatalf("%s: %v", want.Slug, err)
			}
			// A restored inbox is a new lifetime of its sequence numbers: the server
			// that runs on the snapshot may accept different messages from the one
			// it was taken from, so a cursor from one must not pass for the other.
			if got.Epoch == "" || got.Epoch == want.Epoch {
				t.Errorf("%s: epoch %q after restore, was %q; a snapshot must renew it", want.Slug, got.Epoch, want.Epoch)
			}
			got.Epoch = want.Epoch
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s differs after restore:\n got  %+v\n want %+v", want.Slug, got, want)
			}
			if !reflect.DeepEqual(all(t, r, got), all(t, s, want)) {
				t.Errorf("%s: the messages differ after restore", want.Slug)
			}
		}
		// The restored Store carries on from where the original was.
		m, _, err := r.Append(ctx, a.ID, &inbox.NewMessage{Method: "POST", Body: []byte("after"), Verified: true})
		if err != nil || m.Seq != 26 {
			t.Errorf("the next append after restore: seq %v, %v", m, err)
		}
	})

	t.Run("AnEmptyStoreCanBeBackedUp", func(t *testing.T) {
		s := newStore(t)
		defer func() { _ = s.Close() }()
		r := restore(t, take(t, s))
		defer func() { _ = r.Close() }()
		if list, err := r.ListInboxes(t.Context(), inbox.ListParams{}); err != nil || len(list) != 0 {
			t.Errorf("an empty store restored as %v, %v", list, err)
		}
	})

	t.Run("ASnapshotIsOneMomentEvenWhileWritesContinue", func(t *testing.T) {
		s := newStore(t)
		defer func() { _ = s.Close() }()
		ctx := t.Context()
		in := &inbox.Inbox{Slug: "backup-busy", IngestKey: "backup-busy", Name: "busy", Verifier: "none"}
		if err := s.CreateInbox(ctx, in); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			if _, _, err := s.Append(ctx, in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("seed"), Verified: true}); err != nil {
				t.Fatal(err)
			}
		}

		var committed atomic.Int64
		committed.Store(50)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 3; w++ {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, _, err := s.Append(ctx, in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("live"), Verified: true}); err == nil {
						committed.Add(1)
					}
				}
			})
		}
		deadline := time.Now().Add(30 * time.Second)
		for committed.Load() < 150 {
			if time.Now().After(deadline) {
				close(stop)
				wg.Wait()
				t.Fatalf("only %d appends committed in 30s; the Store is not accepting writes", committed.Load())
			}
			time.Sleep(time.Millisecond)
		}
		before := committed.Load() // committed before the backup began
		data := take(t, s)
		close(stop)
		wg.Wait()

		r := restore(t, data)
		defer func() { _ = r.Close() }()
		got, err := r.GetInboxBySlug(ctx, in.Slug)
		if err != nil {
			t.Fatal(err)
		}
		msgs := all(t, r, got)
		if int64(len(msgs)) < before {
			t.Errorf("the snapshot holds %d messages, but %d were committed before it began", len(msgs), before)
		}
		for i, m := range msgs {
			if m.Seq != int64(i+1) {
				t.Fatalf("message %d has seq %d: the snapshot has a gap", i, m.Seq)
			}
		}
		if got.LastSeq != int64(len(msgs)) {
			t.Errorf("last_seq is %d but the snapshot holds %d messages: it mixes two moments", got.LastSeq, len(msgs))
		}
	})

	t.Run("AppendsAndReadsContinueWhileASnapshotIsHeld", func(t *testing.T) {
		s := newStore(t)
		defer func() { _ = s.Close() }()
		ctx := t.Context()
		in := &inbox.Inbox{Slug: "backup-held", IngestKey: "backup-held", Name: "held", Verifier: "none"}
		if err := s.CreateInbox(ctx, in); err != nil {
			t.Fatal(err)
		}
		rc, _, err := s.(inbox.Backupper).Backup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The snapshot is taken and not yet read or closed: the Store must not
		// be waiting on whoever holds it.
		done := make(chan error, 1)
		go func() {
			_, _, err := s.Append(ctx, in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("during"), Verified: true})
			if err == nil {
				_, err = s.Query(ctx, in.ID, inbox.Query{})
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("an append or read while a snapshot is held: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("an append blocked while a snapshot was held")
		}
		_ = rc.Close()
	})

	t.Run("ABackupOfAClosedStoreFails", func(t *testing.T) {
		s := newStore(t)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if rc, _, err := s.(inbox.Backupper).Backup(t.Context()); err == nil {
			_ = rc.Close()
			t.Error("a closed Store produced a backup")
		}
	})

	t.Run("TheSnapshotIsIndependentOfTheStore", func(t *testing.T) {
		s := newStore(t)
		defer func() { _ = s.Close() }()
		in := &inbox.Inbox{Slug: "backup-indep", IngestKey: "backup-indep", Name: "x", Verifier: "none"}
		if err := s.CreateInbox(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		data := take(t, s)
		if _, _, err := s.Append(t.Context(), in.ID, &inbox.NewMessage{Method: "POST", Body: []byte("later"), Verified: true}); err != nil {
			t.Fatal(err)
		}
		r := restore(t, data)
		defer func() { _ = r.Close() }()
		got, _ := r.GetInboxBySlug(t.Context(), in.Slug)
		if got == nil || got.LastSeq != 0 || len(all(t, r, got)) != 0 {
			t.Error("a message appended after the backup appeared in it")
		}
	})
}
