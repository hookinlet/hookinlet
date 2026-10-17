// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// testPruneCancelledPartway cancels a Prune once it has deleted some messages. It
// must return promptly, with the context's error and the number it deleted, and
// leave every inbox as a prune would: its remaining messages a contiguous run up to
// LatestSeq, with Stats and Query agreeing, and nothing else changed. Running Prune
// again finishes the job. (The CancelledContext case covers a context that is
// cancelled before the call.)
//
// The cancel is made as soon as the first inbox's oldest message has moved, which a
// fast Store may already have finished pruning by then; the consistency checks hold
// either way, and the case logs which it was.
func testPruneCancelledPartway(t *testing.T, s inbox.Store, clk *Clock) {
	const count, perInbox, keep = 2, 700, 10
	const due = count * (perInbox - keep)
	var inboxes []*inbox.Inbox
	for i := range count {
		in := createInbox(t, s, fmt.Sprintf("prune-cancel-%d", i), func(in *inbox.Inbox) { in.RetentionMaxCount = keep })
		appendN(t, s, in.ID, perInbox, nil)
		inboxes = append(inboxes, in)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		deleted int64
		err     error
	}
	done := make(chan result, 1)
	go func() {
		n, err := s.Prune(ctx, clk.Now())
		done <- result{n, err}
	}()

	// Cancel once the first batch is visible, or when Prune is done anyway.
	var res result
	var cancelledAt time.Time
	finishedFirst := false
watch:
	for {
		select {
		case res = <-done:
			finishedFirst = true
			cancel()
			break watch
		default:
		}
		if st, err := s.Stats(t.Context(), inboxes[0].ID); err == nil && st.OldestSeq > 1 {
			cancel()
			cancelledAt = time.Now()
			break watch
		}
		time.Sleep(100 * time.Microsecond)
	}
	if !finishedFirst {
		select {
		case res = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Prune had not returned 10s after its context was cancelled")
		}
		t.Logf("Prune returned %v after the cancel: deleted %d of %d, error %v", time.Since(cancelledAt).Round(time.Millisecond), res.deleted, due, res.err)
	} else {
		t.Logf("Prune finished before it could be cancelled: deleted %d of %d", res.deleted, due)
	}
	if res.err != nil && !errors.Is(res.err, context.Canceled) {
		t.Errorf("Prune cancelled partway: error %v; want one wrapping context.Canceled", res.err)
	}
	if res.err == nil && res.deleted != due {
		t.Errorf("Prune returned no error but deleted %d of the %d due", res.deleted, due)
	}

	// Each inbox is in a state a prune leaves: a contiguous run of its newest
	// messages, ending at LatestSeq, which Stats and Query agree on.
	check := func(when string) int64 {
		t.Helper()
		var gone int64
		for _, in := range inboxes {
			st, err := s.Stats(t.Context(), in.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st.LatestSeq != perInbox || st.OldestSeq < 1 || st.OldestSeq > perInbox-keep+1 ||
				st.MessageCount != st.LatestSeq-st.OldestSeq+1 {
				t.Errorf("%s, %s: stats %+v; want the newest messages from oldest_seq to %d, contiguous", when, in.Slug, st, perInbox)
				continue
			}
			got := walkAscending(t, s, in.ID, inbox.Query{}, inbox.MaxQueryLimit)
			if want := seqRange(st.OldestSeq, st.LatestSeq); !reflect.DeepEqual(got, want) {
				t.Errorf("%s, %s: %d messages readable, from %v; want exactly seqs %d..%d", when, in.Slug, len(got), got[:min(len(got), 3)], st.OldestSeq, st.LatestSeq)
			}
			if p := query(t, s, in.ID, inbox.Query{Limit: 1}); p.OldestSeq != st.OldestSeq || p.LatestSeq != st.LatestSeq {
				t.Errorf("%s, %s: a page says oldest %d and latest %d; Stats says %d and %d", when, in.Slug, p.OldestSeq, p.LatestSeq, st.OldestSeq, st.LatestSeq)
			}
			gone += st.OldestSeq - 1
		}
		return gone
	}
	if gone := check("after the cancelled Prune"); gone != res.deleted {
		t.Errorf("the cancelled Prune reported %d deleted, but %d messages are gone", res.deleted, gone)
	}

	// Pruning again finishes the job, and counts only what it deleted itself.
	rest, err := s.Prune(t.Context(), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.deleted+rest != due {
		t.Errorf("the two Prunes deleted %d and %d; want %d together", res.deleted, rest, due)
	}
	if gone := check("after pruning again"); gone != due {
		t.Errorf("after pruning again %d messages are gone; want %d", gone, due)
	}
	for _, in := range inboxes {
		if m := appendMsg(t, s, in.ID); m.Seq != perInbox+1 {
			t.Errorf("%s: the next message got seq %d, want %d", in.Slug, m.Seq, perInbox+1)
		}
	}
}
