// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// ---- query ----

// seedQuery creates an inbox with 30 messages. Message i arrives i minutes
// after baseTime, has event type a, b, or c in rotation, is verified when i is
// even, and has delivery id d<i>.
func seedQuery(t *testing.T, s inbox.Store, clk *Clock) *inbox.Inbox {
	t.Helper()
	in := createInbox(t, s, "query-fixture", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	other := createInbox(t, s, "query-noise")
	for i := 1; i <= 30; i++ {
		at := baseTime.Add(time.Duration(i) * time.Minute)
		appendAt(t, s, clk, in.ID, at, func(m *inbox.NewMessage) {
			m.Body = []byte(fmt.Sprintf("message-%d", i))
			m.EventType = []string{"a", "b", "c"}[(i-1)%3]
			m.Verified = i%2 == 0
			m.DeliveryID = fmt.Sprintf("d%d", i)
		})
		// Messages in another inbox must never show up in these results.
		appendAt(t, s, clk, other.ID, at, func(m *inbox.NewMessage) { m.EventType = "a" })
	}
	return in
}

// seqsWhere returns, ascending, the seqs in 1..30 for which keep is true.
func seqsWhere(keep func(i int64) bool) []int64 {
	var out []int64
	for i := int64(1); i <= 30; i++ {
		if keep(i) {
			out = append(out, i)
		}
	}
	return out
}

func descending(in []int64) []int64 {
	out := slices.Clone(in)
	slices.Reverse(out)
	return out
}

func testQueryFilters(t *testing.T, s inbox.Store, clk *Clock) {
	in := seedQuery(t, s, clk)
	q := func(q inbox.Query) *inbox.Page { return query(t, s, in.ID, q) }
	typeIs := func(want ...string) func(i int64) bool {
		return func(i int64) bool { return slices.Contains(want, []string{"a", "b", "c"}[(i-1)%3]) }
	}
	all := seqsWhere(func(int64) bool { return true })

	// Default order is newest first.
	p := q(inbox.Query{})
	wantSeqs(t, "default", p.Messages, descending(all))
	if p.LatestSeq != 30 {
		t.Errorf("LatestSeq = %d, want 30", p.LatestSeq)
	}

	wantSeqs(t, "limit 5, newest first", q(inbox.Query{Limit: 5}).Messages, seqRangeDesc(30, 26))
	wantSeqs(t, "after 0", q(inbox.Query{After: ptr(int64(0)), Limit: 5}).Messages, seqRange(1, 5))
	wantSeqs(t, "after 5", q(inbox.Query{After: ptr(int64(5)), Limit: 5}).Messages, seqRange(6, 10))
	wantSeqs(t, "after 28", q(inbox.Query{After: ptr(int64(28))}).Messages, seqRange(29, 30))
	wantSeqs(t, "after latest", q(inbox.Query{After: ptr(int64(30))}).Messages, nil)
	wantSeqs(t, "after far beyond", q(inbox.Query{After: ptr(int64(1 << 40))}).Messages, nil)
	wantSeqs(t, "before 10", q(inbox.Query{Before: ptr(int64(10)), Limit: 3}).Messages, seqRangeDesc(9, 7))
	wantSeqs(t, "before 1", q(inbox.Query{Before: ptr(int64(1))}).Messages, nil)
	wantSeqs(t, "before 0", q(inbox.Query{Before: ptr(int64(0))}).Messages, nil)
	wantSeqs(t, "before beyond latest", q(inbox.Query{Before: ptr(int64(1000)), Limit: 2}).Messages, seqRangeDesc(30, 29))
	if got := q(inbox.Query{After: ptr(int64(30))}).LatestSeq; got != 30 {
		t.Errorf("an empty page still reports LatestSeq: got %d, want 30", got)
	}

	// Event types combine with OR.
	wantSeqs(t, "type a", q(inbox.Query{Types: []string{"a"}, After: ptr(int64(0))}).Messages, seqsWhere(typeIs("a")))
	wantSeqs(t, "types a,c", q(inbox.Query{Types: []string{"a", "c"}, After: ptr(int64(0))}).Messages, seqsWhere(typeIs("a", "c")))
	wantSeqs(t, "unknown type", q(inbox.Query{Types: []string{"zzz"}}).Messages, nil)

	// Since is inclusive, Until exclusive.
	at := func(minutes int) time.Time { return baseTime.Add(time.Duration(minutes) * time.Minute) }
	wantSeqs(t, "since 10", q(inbox.Query{Since: at(10), After: ptr(int64(0))}).Messages, seqRange(10, 30))
	wantSeqs(t, "until 20", q(inbox.Query{Until: at(20), After: ptr(int64(0))}).Messages, seqRange(1, 19))
	wantSeqs(t, "since 10 until 20", q(inbox.Query{Since: at(10), Until: at(20)}).Messages, seqRangeDesc(19, 10))
	wantSeqs(t, "since between messages", q(inbox.Query{Since: at(10).Add(time.Second), After: ptr(int64(0)), Limit: 2}).Messages, seqRange(11, 12))
	wantSeqs(t, "since in a different zone", q(inbox.Query{Since: at(25).In(time.FixedZone("X", 5*3600)), After: ptr(int64(0))}).Messages, seqRange(25, 30))

	// Verified, delivery id.
	wantSeqs(t, "verified", q(inbox.Query{Verified: ptr(true), After: ptr(int64(0))}).Messages, seqsWhere(func(i int64) bool { return i%2 == 0 }))
	wantSeqs(t, "unverified", q(inbox.Query{Verified: ptr(false), After: ptr(int64(0))}).Messages, seqsWhere(func(i int64) bool { return i%2 == 1 }))
	wantSeqs(t, "delivery id", q(inbox.Query{DeliveryID: "d7"}).Messages, []int64{7})
	wantSeqs(t, "missing delivery id", q(inbox.Query{DeliveryID: "d999"}).Messages, nil)

	// Everything together: type a, verified, after 5, until minute 25.
	want := seqsWhere(func(i int64) bool { return i > 5 && i < 25 && i%2 == 0 && typeIs("a")(i) })
	got := q(inbox.Query{After: ptr(int64(5)), Types: []string{"a"}, Verified: ptr(true), Until: at(25)})
	wantSeqs(t, "combined filters", got.Messages, want)

	// Results carry the full message.
	m := q(inbox.Query{DeliveryID: "d7"}).Messages[0]
	if m.InboxID != in.ID || m.EventType != "a" || m.Verified || string(m.Body) != "message-7" ||
		!m.ReceivedAt.Equal(at(7)) || m.Size != int64(len("message-7")) {
		t.Errorf("unexpected message: %+v", *m)
	}

	// An inbox with no messages is an empty page, not an error.
	empty := createInbox(t, s, "empty-inbox")
	if p := query(t, s, empty.ID, inbox.Query{}); len(p.Messages) != 0 || p.LatestSeq != 0 {
		t.Errorf("empty inbox page = %+v", p)
	}
}

func testQueryBodyLimit(t *testing.T, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "body-limit")
	sizes := []int{0, 5, 20, 21, 100}
	for _, n := range sizes {
		appendMsg(t, s, in.ID, func(m *inbox.NewMessage) { m.Body = make([]byte, n) })
	}
	byLimit := func(limit *int64) []*inbox.Message {
		return query(t, s, in.ID, inbox.Query{After: ptr(int64(0)), BodyLimit: limit}).Messages
	}
	check := func(what string, limit *int64, wantBody func(size int) bool) {
		t.Helper()
		msgs := byLimit(limit)
		if len(msgs) != len(sizes) {
			t.Fatalf("%s: %d messages", what, len(msgs))
		}
		for i, m := range msgs {
			if m.Size != int64(sizes[i]) {
				t.Errorf("%s: seq %d Size = %d, want %d", what, m.Seq, m.Size, sizes[i])
			}
			if has := m.Body != nil; has != wantBody(sizes[i]) {
				t.Errorf("%s: seq %d (size %d) body present = %v, want %v", what, m.Seq, sizes[i], has, wantBody(sizes[i]))
			}
			if m.Body != nil && len(m.Body) != sizes[i] {
				t.Errorf("%s: seq %d body length %d, want %d", what, m.Seq, len(m.Body), sizes[i])
			}
		}
	}
	check("no limit", nil, func(int) bool { return true })
	check("limit 20", ptr(int64(20)), func(n int) bool { return n <= 20 })
	check("limit 0", ptr(int64(0)), func(n int) bool { return n == 0 })
	check("huge limit", ptr(int64(1<<40)), func(int) bool { return true })

	// Get always returns the full body.
	if m, err := s.Get(t.Context(), in.ID, 5); err != nil || len(m.Body) != 100 {
		t.Errorf("Get returned %d body bytes, %v; want 100", len(m.Body), err)
	}
}

func testQueryLimits(t *testing.T, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "limits")
	const n = 520
	for range n {
		appendMsg(t, s, in.ID, func(m *inbox.NewMessage) { m.Body = nil })
	}
	count := func(q inbox.Query) int { return len(query(t, s, in.ID, q).Messages) }
	if got := count(inbox.Query{}); got != inbox.DefaultQueryLimit {
		t.Errorf("default limit returned %d, want %d", got, inbox.DefaultQueryLimit)
	}
	if got := count(inbox.Query{Limit: 1}); got != 1 {
		t.Errorf("limit 1 returned %d", got)
	}
	if got := count(inbox.Query{Limit: 10_000}); got != inbox.MaxQueryLimit {
		t.Errorf("huge limit returned %d, want it clamped to %d", got, inbox.MaxQueryLimit)
	}
	wantSeqs(t, "newest 50", query(t, s, in.ID, inbox.Query{}).Messages, seqRangeDesc(n, n-49))
	wantSeqs(t, "clamped ascending", query(t, s, in.ID, inbox.Query{After: ptr(int64(0)), Limit: 10_000}).Messages, seqRange(1, 500))
}

func testQueryInvalid(t *testing.T, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "invalid-queries")
	appendN(t, s, in.ID, 3, nil)
	t0 := baseTime
	bad := map[string]inbox.Query{
		"after and before":   {After: ptr(int64(1)), Before: ptr(int64(3))},
		"negative after":     {After: ptr(int64(-1))},
		"negative before":    {Before: ptr(int64(-1))},
		"negative limit":     {Limit: -1},
		"negative body":      {BodyLimit: ptr(int64(-1))},
		"empty event type":   {Types: []string{"a", ""}},
		"since after until":  {Since: t0.Add(time.Hour), Until: t0},
		"since equals until": {Since: t0, Until: t0},
		"too many types":     {Types: make([]string, inbox.MaxQueryTypes+1)},
	}
	for i := range bad["too many types"].Types {
		bad["too many types"].Types[i] = fmt.Sprintf("t%d", i)
	}
	for name, q := range bad {
		_, err := s.Query(t.Context(), in.ID, q)
		isErr(t, name, err, inbox.ErrInvalid)
	}
}

// walk reads the whole inbox through the public paging protocol and returns
// every seq in the order seen.
func walkAscending(t *testing.T, s inbox.Store, id inbox.ID, base inbox.Query, limit int) []int64 {
	t.Helper()
	var out []int64
	cursor := int64(0)
	for iterations := 0; ; iterations++ {
		if iterations > 100_000 {
			t.Fatal("paging does not terminate")
		}
		q := base
		q.After, q.Limit = ptr(cursor), limit
		page := query(t, s, id, q)
		if len(page.Messages) == 0 {
			return out
		}
		for _, m := range page.Messages {
			if m.Seq <= cursor {
				t.Fatalf("page went backwards: seq %d after cursor %d", m.Seq, cursor)
			}
			cursor = m.Seq
			out = append(out, m.Seq)
		}
	}
}

func walkDescending(t *testing.T, s inbox.Store, id inbox.ID, base inbox.Query, limit int) []int64 {
	t.Helper()
	var out []int64
	var cursor *int64
	for iterations := 0; ; iterations++ {
		if iterations > 100_000 {
			t.Fatal("paging does not terminate")
		}
		q := base
		q.Before, q.Limit = cursor, limit
		page := query(t, s, id, q)
		if len(page.Messages) == 0 {
			return out
		}
		for _, m := range page.Messages {
			if cursor != nil && m.Seq >= *cursor {
				t.Fatalf("page went forwards: seq %d before cursor %d", m.Seq, *cursor)
			}
			c := m.Seq
			cursor = &c
			out = append(out, m.Seq)
		}
	}
}

func testQueryPaging(t *testing.T, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "paging")
	const n = 250
	appendN(t, s, in.ID, n, func(i int, m *inbox.NewMessage) {
		m.EventType = []string{"x", "y", "z"}[i%3]
	})

	for _, limit := range []int{1, 7, 50, 500} {
		if got := walkAscending(t, s, in.ID, inbox.Query{}, limit); !reflect.DeepEqual(got, seqRange(1, n)) {
			t.Errorf("ascending walk (limit %d) saw %d seqs, not exactly 1..%d once each", limit, len(got), n)
		}
		if got := walkDescending(t, s, in.ID, inbox.Query{}, limit); !reflect.DeepEqual(got, seqRangeDesc(n, 1)) {
			t.Errorf("descending walk (limit %d) saw %d seqs, not exactly %d..1 once each", limit, len(got), n)
		}
	}

	var wantX []int64
	for i := int64(1); i <= n; i++ {
		if i%3 == 0 {
			wantX = append(wantX, i)
		}
	}
	filtered := inbox.Query{Types: []string{"x"}}
	if got := walkAscending(t, s, in.ID, filtered, 11); !reflect.DeepEqual(got, wantX) {
		t.Errorf("filtered ascending walk saw %d seqs, want %d", len(got), len(wantX))
	}
	if got := walkDescending(t, s, in.ID, filtered, 11); !reflect.DeepEqual(got, descending(wantX)) {
		t.Errorf("filtered descending walk saw %d seqs, want %d", len(got), len(wantX))
	}
}

// ---- retention ----

func testRetentionByAge(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	now := baseTime.Add(30 * 24 * time.Hour)
	day := 24 * time.Hour

	aged := createInbox(t, s, "by-age", func(in *inbox.Inbox) { in.RetentionDays = 2 })
	forever := createInbox(t, s, "keep-forever")
	for _, ago := range []time.Duration{5 * day, 3 * day, 2 * day, day, 0} {
		appendAt(t, s, clk, aged.ID, now.Add(-ago))
		appendAt(t, s, clk, forever.ID, now.Add(-ago))
	}

	deleted, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	// Strictly older than two days goes; the message at exactly two days stays.
	if deleted != 2 {
		t.Errorf("deleted %d, want 2", deleted)
	}
	wantSeqs(t, "after age prune", query(t, s, aged.ID, inbox.Query{After: ptr(int64(0))}).Messages, seqRange(3, 5))
	if st, _ := s.Stats(ctx, aged.ID); *st != (inbox.InboxStats{MessageCount: 3, OldestSeq: 3, LatestSeq: 5}) {
		t.Errorf("stats after prune = %+v", st)
	}
	if walked := len(walkAscending(t, s, aged.ID, inbox.Query{}, 100)); walked != 3 {
		t.Errorf("Stats says 3 messages but %d are stored", walked)
	}
	if st, _ := s.Stats(ctx, forever.ID); st.MessageCount != 5 {
		t.Errorf("an inbox with no retention lost messages: %+v", st)
	}
	if _, err := s.Get(ctx, aged.ID, 1); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("Get of a pruned message: %v", err)
	}

	// Sequence numbers are never reused.
	if m := appendAt(t, s, clk, aged.ID, now); m.Seq != 6 {
		t.Errorf("Seq after pruning = %d, want 6", m.Seq)
	}
	if got, _ := s.GetInbox(ctx, aged.ID); got.LastSeq != 6 {
		t.Errorf("LastSeq = %d, want 6", got.LastSeq)
	}

	// Pruning again changes nothing.
	if again, err := s.Prune(ctx, now); err != nil || again != 0 {
		t.Errorf("second prune deleted %d, %v; want 0", again, err)
	}
	// Later, the boundary message ages out.
	if later, err := s.Prune(ctx, now.Add(time.Nanosecond+time.Microsecond)); err != nil || later != 1 {
		t.Errorf("prune a moment later deleted %d, %v; want 1 (the message at exactly two days)", later, err)
	}
}

func testRetentionByCount(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	capped := createInbox(t, s, "by-count", func(in *inbox.Inbox) { in.RetentionMaxCount = 3 })
	under := createInbox(t, s, "under-count", func(in *inbox.Inbox) { in.RetentionMaxCount = 100 })
	unlimited := createInbox(t, s, "no-limit")
	appendN(t, s, capped.ID, 10, nil)
	appendN(t, s, under.ID, 10, nil)
	appendN(t, s, unlimited.ID, 10, nil)

	deleted, err := s.Prune(ctx, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 7 {
		t.Errorf("deleted %d, want 7", deleted)
	}
	wantSeqs(t, "newest three kept", query(t, s, capped.ID, inbox.Query{After: ptr(int64(0))}).Messages, seqRange(8, 10))
	for _, in := range []*inbox.Inbox{under, unlimited} {
		if st, _ := s.Stats(ctx, in.ID); st.MessageCount != 10 {
			t.Errorf("inbox %s lost messages: %+v", in.Slug, st)
		}
	}
	if m := appendMsg(t, s, capped.ID); m.Seq != 11 {
		t.Errorf("Seq after pruning = %d, want 11", m.Seq)
	}

	// A prune bigger than any internal batch size still completes. Remove the
	// other capped inbox first so its leftovers do not count toward the total.
	if err := s.DeleteInbox(ctx, capped.ID); err != nil {
		t.Fatal(err)
	}
	big := createInbox(t, s, "big-prune", func(in *inbox.Inbox) { in.RetentionMaxCount = 100 })
	const total = 5100
	for range total {
		appendMsg(t, s, big.ID, func(m *inbox.NewMessage) { m.Body = nil })
	}
	deleted, err = s.Prune(ctx, baseTime)
	if err != nil || deleted != total-100 {
		t.Fatalf("big prune deleted %d, %v; want %d", deleted, err, total-100)
	}
	if st, _ := s.Stats(ctx, big.ID); *st != (inbox.InboxStats{MessageCount: 100, OldestSeq: total - 99, LatestSeq: total}) {
		t.Errorf("stats after big prune = %+v", st)
	}
}

func testRetentionCombined(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	now := baseTime.Add(100 * 24 * time.Hour)
	day := 24 * time.Hour
	in := createInbox(t, s, "combined", func(in *inbox.Inbox) {
		in.RetentionDays = 2
		in.RetentionMaxCount = 3
	})
	// Ages in days before now, oldest first. The age rule removes seqs 1-2;
	// the count rule additionally removes seq 3 (only 4, 5, 6 are newest).
	for _, ago := range []time.Duration{10 * day, 9 * day, day, day / 2, day / 3, day / 4} {
		appendAt(t, s, clk, in.ID, now.Add(-ago))
	}
	deleted, err := s.Prune(ctx, now)
	if err != nil || deleted != 3 {
		t.Fatalf("deleted %d, %v; want 3", deleted, err)
	}
	wantSeqs(t, "union of both rules", query(t, s, in.ID, inbox.Query{After: ptr(int64(0))}).Messages, seqRange(4, 6))

	// When the age rule alone is stricter, the count rule must not resurrect or
	// delete anything extra.
	strict := createInbox(t, s, "combined-strict-age", func(in *inbox.Inbox) {
		in.RetentionDays = 1
		in.RetentionMaxCount = 100
	})
	for _, ago := range []time.Duration{5 * day, 4 * day, 3 * day} {
		appendAt(t, s, clk, strict.ID, now.Add(-ago))
	}
	if deleted, err := s.Prune(ctx, now); err != nil || deleted != 3 {
		t.Errorf("age-only case deleted %d, %v; want 3", deleted, err)
	}
	if st, _ := s.Stats(ctx, strict.ID); st.MessageCount != 0 || st.OldestSeq != 0 || st.LatestSeq != 3 {
		t.Errorf("fully pruned inbox stats = %+v", st)
	}
}

// What retention removes is always a prefix of the inbox: ReceivedAt never
// decreases with Seq, so no old message can sit behind a surviving newer one.
func testRetentionIsPrefix(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	now := baseTime.Add(100 * time.Hour)
	in := createInbox(t, s, "prefix", func(in *inbox.Inbox) { in.RetentionDays = 1 })

	a := appendAt(t, s, clk, in.ID, now.Add(-72*time.Hour))
	b := appendAt(t, s, clk, in.ID, now.Add(-100*time.Hour)) // the clock stepped far back
	c := appendAt(t, s, clk, in.ID, now.Add(-time.Hour))
	d := appendAt(t, s, clk, in.ID, now.Add(-2*time.Hour)) // and back again
	if b.ReceivedAt.Before(a.ReceivedAt) || d.ReceivedAt.Before(c.ReceivedAt) {
		t.Fatalf("ReceivedAt is not monotonic: %v %v %v %v", a.ReceivedAt, b.ReceivedAt, c.ReceivedAt, d.ReceivedAt)
	}

	deleted, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d, want the first two", deleted)
	}
	remaining := seqsOf(query(t, s, in.ID, inbox.Query{After: ptr(int64(0))}).Messages)
	if !reflect.DeepEqual(remaining, []int64{3, 4}) {
		t.Errorf("remaining seqs = %v, want [3 4]: pruning must remove a prefix, not leave gaps", remaining)
	}
	st, _ := s.Stats(ctx, in.ID)
	if st.MessageCount != st.LatestSeq-st.OldestSeq+1 {
		t.Errorf("stored seqs are not contiguous: %+v", st)
	}
	// And the reported count matches the messages actually there.
	if walked := int64(len(walkAscending(t, s, in.ID, inbox.Query{}, 100))); walked != st.MessageCount {
		t.Errorf("Stats reports %d messages but %d are stored", st.MessageCount, walked)
	}
}

func testRetentionAndDedupe(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "prune-dedupe", func(in *inbox.Inbox) { in.RetentionMaxCount = 1 })
	first := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) { m.DeliveryID = "evt_prune" })
	appendMsg(t, s, in.ID)
	if deleted, err := s.Prune(ctx, clk.Now()); err != nil || deleted != 1 {
		t.Fatalf("prune: %d, %v", deleted, err)
	}

	again, created, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) { m.DeliveryID = "evt_prune" }))
	if err != nil {
		t.Fatal(err)
	}
	if !created || again.Seq == first.Seq || again.Seq != 3 {
		t.Errorf("after pruning, re-appending the delivery: created=%v seq=%d; want a new message with seq 3", created, again.Seq)
	}
}

// ---- concurrency ----

func testConcurrentGapless(t *testing.T, s inbox.Store, _ *Clock) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const goroutines, perGoroutine = 50, 20 // 1,000 appends across two inboxes
	inboxes := []*inbox.Inbox{createInbox(t, s, "gapless-a"), createInbox(t, s, "gapless-b")}

	var mu sync.Mutex
	got := make([][]int64, len(inboxes))
	var firstErr error
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			target := g % len(inboxes)
			for i := range perGoroutine {
				m, created, err := s.Append(ctx, inboxes[target].ID, nm(func(m *inbox.NewMessage) {
					m.Body = []byte(fmt.Sprintf("g%d-%d", g, i))
				}))
				mu.Lock()
				switch {
				case err != nil:
					if firstErr == nil {
						firstErr = fmt.Errorf("append failed (any error, including a busy database, is a failure): %w", err)
					}
				case !created:
					if firstErr == nil {
						firstErr = errors.New("a message without a delivery id was reported as a duplicate")
					}
				default:
					got[target] = append(got[target], m.Seq)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatal(firstErr)
	}

	perInbox := goroutines / len(inboxes) * perGoroutine
	for i, in := range inboxes {
		slices.Sort(got[i])
		if !reflect.DeepEqual(got[i], seqRange(1, int64(perInbox))) {
			t.Errorf("inbox %s: assigned seqs are not exactly 1..%d once each (got %d values)", in.Slug, perInbox, len(got[i]))
		}
		msgs := walkAscending(t, s, in.ID, inbox.Query{}, 500)
		if len(msgs) != perInbox {
			t.Errorf("inbox %s: %d messages readable, want %d", in.Slug, len(msgs), perInbox)
		}
		if st, _ := s.Stats(ctx, in.ID); st.MessageCount != int64(perInbox) || st.LatestSeq != int64(perInbox) {
			t.Errorf("inbox %s stats = %+v", in.Slug, st)
		}
	}
}

// A poller walking the inbox with After() while writers append concurrently
// must see every message exactly once and in order: no gaps, and no message
// appearing behind a cursor the poller already passed.
func testCursorSafety(t *testing.T, s inbox.Store, _ *Clock) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const writers, perWriter = 8, 125
	const total = writers * perWriter
	in := createInbox(t, s, "cursor-safety")

	var failOnce sync.Once
	var failure error
	fail := func(err error) {
		failOnce.Do(func() {
			failure = err
			cancel()
		})
	}

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				_, _, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) {
					m.Body = []byte(fmt.Sprintf("w%d-%d", w, i))
				}))
				if err != nil {
					fail(fmt.Errorf("writer %d: %w", w, err))
					return
				}
			}
		})
	}

	seen := make(map[string]int, total)
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		cursor := int64(0)
		for cursor < total {
			if ctx.Err() != nil {
				return
			}
			after := cursor
			page, err := s.Query(ctx, in.ID, inbox.Query{After: &after, Limit: 50})
			if err != nil {
				fail(fmt.Errorf("poller: %w", err))
				return
			}
			for _, m := range page.Messages {
				if m.Seq != cursor+1 {
					fail(fmt.Errorf("poller saw seq %d right after %d: a message was skipped or appeared late", m.Seq, cursor))
					return
				}
				cursor = m.Seq
				seen[string(m.Body)]++
			}
			if len(page.Messages) == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	wg.Wait()
	<-pollDone
	if failure != nil {
		t.Fatal(failure)
	}
	if len(seen) != total {
		t.Fatalf("poller saw %d distinct messages, want %d", len(seen), total)
	}
	for body, n := range seen {
		if n != 1 {
			t.Errorf("message %q seen %d times", body, n)
		}
	}
}

// Updating an inbox never rewinds or skips its sequence counter, even when the
// updater holds a stale copy of the inbox from before the appends began.
func testUpdateDuringAppends(t *testing.T, s inbox.Store, _ *Clock) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const writers, perWriter, updates = 4, 100, 100
	in := createInbox(t, s, "update-race")

	var failOnce sync.Once
	var failure error
	fail := func(err error) { failOnce.Do(func() { failure = err; cancel() }) }

	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range perWriter {
				if _, _, err := s.Append(ctx, in.ID, nm()); err != nil {
					fail(fmt.Errorf("append: %w", err))
					return
				}
			}
		})
	}
	wg.Go(func() {
		stale := *in // LastSeq 0, from before any append
		for i := range updates {
			u := stale
			cur, err := s.GetInbox(ctx, in.ID)
			if err != nil {
				fail(fmt.Errorf("get: %w", err))
				return
			}
			u.Version = cur.Version // only the sequence-bearing fields stay stale
			u.Name = fmt.Sprintf("name-%d", i)
			if err := s.UpdateInbox(ctx, &u); err != nil {
				fail(fmt.Errorf("update %d: %w", i, err))
				return
			}
		}
	})
	wg.Wait()
	if failure != nil {
		t.Fatal(failure)
	}

	const total = writers * perWriter
	if seq, _ := s.LatestSeq(ctx, in.ID); seq != total {
		t.Errorf("LatestSeq = %d, want %d: an update disturbed the counter", seq, total)
	}
	if got := walkAscending(t, s, in.ID, inbox.Query{}, 500); !reflect.DeepEqual(got, seqRange(1, total)) {
		t.Errorf("seqs are not exactly 1..%d once each (got %d)", total, len(got))
	}
	if got, _ := s.GetInbox(ctx, in.ID); got.Name != fmt.Sprintf("name-%d", updates-1) || got.LastSeq != total {
		t.Errorf("final inbox: name=%q lastSeq=%d", got.Name, got.LastSeq)
	}
}

// A caller that gets a short ascending page may continue after the newest Seq it
// knows of, skipping every row in between. That is only safe if a short page means
// "nothing more matches".
func testQueryExactness(t *testing.T, s inbox.Store, clk *Clock) {
	in := createInbox(t, s, "exactness")
	const n = 120
	for i := 1; i <= n; i++ {
		appendAt(t, s, clk, in.ID, baseTime.Add(time.Duration(i)*time.Second), func(m *inbox.NewMessage) {
			m.EventType = []string{"a", "b", "c"}[i%3]
			m.Verified = i%4 != 0
			m.Body = bytes.Repeat([]byte("x"), i) // varied sizes
		})
	}
	matching := func(q inbox.Query) int {
		count := 0
		for i := 1; i <= n; i++ {
			if len(q.Types) > 0 && !slices.Contains(q.Types, []string{"a", "b", "c"}[i%3]) {
				continue
			}
			if q.Verified != nil && (i%4 != 0) != *q.Verified {
				continue
			}
			count++
		}
		return count
	}
	for name, q := range map[string]inbox.Query{
		"unfiltered":    {},
		"one type":      {Types: []string{"a"}},
		"two types":     {Types: []string{"a", "c"}},
		"verified":      {Verified: ptr(true)},
		"unverified":    {Verified: ptr(false)},
		"type+verified": {Types: []string{"b"}, Verified: ptr(true)},
	} {
		for _, limit := range []int{1, 2, 7, 40, 119, 120, 500} {
			for _, mode := range []string{"after", "newest"} {
				q := q
				q.Limit = limit
				if mode == "after" {
					q.After = ptr(int64(0))
				}
				page := query(t, s, in.ID, q)
				want := min(limit, matching(q))
				if len(page.Messages) != want {
					t.Errorf("%s limit %d (%s): got %d messages, want exactly min(limit, matches) = %d", name, limit, mode, len(page.Messages), want)
				}
			}
		}
	}
	// The limit is clamped to the maximum, and the page is exactly that size.
	for range 450 {
		appendMsg(t, s, in.ID)
	}
	if page := query(t, s, in.ID, inbox.Query{After: ptr(int64(0)), Limit: 100_000}); len(page.Messages) != inbox.MaxQueryLimit {
		t.Errorf("a limit above the maximum returned %d messages, want %d", len(page.Messages), inbox.MaxQueryLimit)
	}
	// Large bodies do not shorten a page: only the caller's limit does.
	big := createInbox(t, s, "exactness-big")
	for range 30 {
		appendMsg(t, s, big.ID, func(m *inbox.NewMessage) { m.Body = bytes.Repeat([]byte("B"), 600<<10) })
	}
	if page := query(t, s, big.ID, inbox.Query{After: ptr(int64(0)), Limit: 30}); len(page.Messages) != 30 {
		t.Errorf("30 messages of 600 KiB came back as %d; a Store may not cut a page short", len(page.Messages))
	}
}

// LatestSeq must come from a state no newer than the rows, or a caller who
// continues after it can skip a message that committed in between. Appends race
// the reads here, so a Store that reads last_seq after the rows fails quickly.
func testQuerySnapshot(t *testing.T, s inbox.Store, _ *Clock) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	in := createInbox(t, s, "snapshot")

	const writers, perWriter = 4, 150
	const total = writers * perWriter
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range perWriter {
				if _, _, err := s.Append(ctx, in.ID, nm()); err != nil {
					cancel()
					return
				}
			}
		})
	}

	// Every message matches, so a short page must reach exactly LatestSeq.
	const limit = 7
	cursor := int64(0)
	for cursor < total && ctx.Err() == nil {
		after := cursor
		page, err := s.Query(ctx, in.ID, inbox.Query{After: &after, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		end := cursor + int64(len(page.Messages))
		if n := len(page.Messages); n > 0 && page.Messages[n-1].Seq > page.LatestSeq {
			t.Fatalf("a message with seq %d came back with LatestSeq %d: LatestSeq is older than the rows", page.Messages[n-1].Seq, page.LatestSeq)
		}
		if len(page.Messages) < limit && end != page.LatestSeq {
			t.Fatalf("a short page after %d returned %d messages (up to %d) but LatestSeq is %d. If LatestSeq is newer than the rows, a caller that skips ahead to it loses the messages in between",
				cursor, len(page.Messages), end, page.LatestSeq)
		}
		cursor = end
	}
	wg.Wait()
	if cursor != total {
		t.Fatalf("read %d of %d messages", cursor, total)
	}
}
