// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

func newStore(t *testing.T) (*sqlite.Store, *inbox.Inbox) {
	t.Helper()
	s, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	in := &inbox.Inbox{Slug: "wire-test", IngestKey: "wire-test", Name: "wire", Verifier: "github", AgentTokenHash: []byte("hash-for-wire-test")}
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	return s, in
}

func add(t *testing.T, s inbox.Store, in *inbox.Inbox, n int, mod func(i int, m *inbox.NewMessage)) {
	t.Helper()
	for i := 1; i <= n; i++ {
		m := &inbox.NewMessage{Method: "POST", Verified: true, Body: []byte(fmt.Sprintf(`{"n":%d}`, i))}
		if mod != nil {
			mod(i, m)
		}
		if _, _, err := s.Append(t.Context(), in.ID, m); err != nil {
			t.Fatal(err)
		}
	}
}

// follow fetches pages by following next links until there are none, and
// returns every seq seen, in order, plus the number of pages.
func follow(t *testing.T, s inbox.Store, in *inbox.Inbox, firstQuery string) (seqs []int64, pages int, bodies map[int64]Message) {
	t.Helper()
	bodies = map[int64]Message{}
	raw := firstQuery
	for iterations := 0; iterations < 10_000; iterations++ {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		req, err := parseMessages(v)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		page, err := FetchPage(t.Context(), s, in, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) > 0 {
			pages++ // the final empty poll of an ascending walk is not a page of results
		}
		for _, m := range page.Messages {
			seqs = append(seqs, m.Seq)
			bodies[m.Seq] = m
		}
		if page.Next == nil {
			return seqs, pages, bodies
		}
		// An ascending poll always has a next link; stop at an empty page.
		if len(page.Messages) == 0 {
			return seqs, pages, bodies
		}
		_, raw, _ = strings.Cut(*page.Next, "?")
	}
	t.Fatal("paging does not terminate")
	return
}

func seqRange(from, to int64, desc bool) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	if desc {
		for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
			out[l], out[r] = out[r], out[l]
		}
	}
	return out
}

func sameSeqs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFollowingNextVisitsEveryMessageOnce(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 260, nil)

	asc, pages, _ := follow(t, s, in, "after=0&limit=50")
	if !sameSeqs(asc, seqRange(1, 260, false)) || pages != 6 {
		t.Errorf("ascending: %d seqs over %d pages", len(asc), pages)
	}
	desc, pages, _ := follow(t, s, in, "limit=50")
	if !sameSeqs(desc, seqRange(1, 260, true)) || pages != 6 {
		t.Errorf("newest first: %d seqs over %d pages", len(desc), pages)
	}
	desc2, _, _ := follow(t, s, in, "before=101&limit=7")
	if !sameSeqs(desc2, seqRange(1, 100, true)) {
		t.Errorf("before=101: %d seqs", len(desc2))
	}
}

func TestEmptyAscendingPageKeepsTheCursor(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 3, nil)
	v, _ := url.ParseQuery("after=3")
	req, _ := parseMessages(v)
	page, err := FetchPage(t.Context(), s, in, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 0 || page.Messages == nil || page.LatestSeq != 3 {
		t.Errorf("page = %+v", page)
	}
	if page.Next == nil || !strings.Contains(*page.Next, "after=3") {
		t.Errorf("next = %v, want the same cursor to poll again", page.Next)
	}
	// And the next poll sees a message that arrives meanwhile.
	add(t, s, in, 1, nil)
	_, raw, _ := strings.Cut(*page.Next, "?")
	v, _ = url.ParseQuery(raw)
	req, _ = parseMessages(v)
	page, _ = FetchPage(t.Context(), s, in, req)
	if len(page.Messages) != 1 || page.Messages[0].Seq != 4 {
		t.Errorf("the poll after the new message got %+v", page.Messages)
	}
}

func TestFiltersAreCarriedAcrossPages(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 100, func(i int, m *inbox.NewMessage) {
		m.EventType = []string{"a", "b", "c", "d"}[i%4]
		m.Verified = i%5 != 0
	})
	seqs, pages, _ := follow(t, s, in, "after=0&limit=10&type=a&type=c&verified=true")
	var want []int64
	for i := int64(1); i <= 100; i++ {
		if (i%4 == 0 || i%4 == 2) && i%5 != 0 {
			want = append(want, i)
		}
	}
	if !sameSeqs(seqs, want) {
		t.Errorf("filtered walk saw %v over %d pages, want %v", seqs, pages, want)
	}
}

func TestSummaryPagesHaveNoBodies(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 5, nil)
	_, _, byseq := follow(t, s, in, "after=0&fields=summary")
	for seq, m := range byseq {
		if m.Body != nil || m.BodyText != nil || m.BodyBase64 != nil {
			t.Errorf("seq %d has a body in a summary page", seq)
		}
		if m.Size == 0 {
			t.Errorf("seq %d lost its size", seq)
		}
	}
	if len(byseq) != 5 {
		t.Errorf("%d messages", len(byseq))
	}
}

func TestLargeBodiesAreLoadedIndividuallyAndIntact(t *testing.T) {
	s, in := newStore(t)
	// Larger than the inline limit, so the page query withholds them.
	payload := func(i int) []byte {
		return bytes.Repeat([]byte{"abcdefgh"[i]}, InlineBodyBytes+1000+i)
	}
	add(t, s, in, 4, func(i int, m *inbox.NewMessage) { m.Body = payload(i - 1) })
	_, _, byseq := follow(t, s, in, "after=0")
	for i := 1; i <= 4; i++ {
		m, ok := byseq[int64(i)]
		if !ok || m.BodyText == nil || *m.BodyText != string(payload(i-1)) {
			t.Errorf("seq %d: body missing or altered", i)
		}
		if m.Size != int64(InlineBodyBytes+1000+i-1) {
			t.Errorf("seq %d: size %d", i, m.Size)
		}
	}
}

func TestBinaryBodiesSurviveAsBase64(t *testing.T) {
	s, in := newStore(t)
	raw := make([]byte, 70_000) // also over the inline limit
	for i := range raw {
		raw[i] = byte(i)
	}
	add(t, s, in, 1, func(_ int, m *inbox.NewMessage) { m.Body = raw })
	_, _, byseq := follow(t, s, in, "after=0")
	m := byseq[1]
	if m.BodyBase64 == nil {
		t.Fatal("no body_base64")
	}
	got, err := base64.StdEncoding.DecodeString(*m.BodyBase64)
	if err != nil || !bytes.Equal(got, raw) {
		t.Error("the binary body did not round trip")
	}
}

func TestPagesAreBoundedByBodyBytes(t *testing.T) {
	s, in := newStore(t)
	const each = 6 << 20 // three of these exceed the 16 MiB budget
	add(t, s, in, 5, func(i int, m *inbox.NewMessage) {
		m.Body = bytes.Repeat([]byte{"ABCDEFGH"[i]}, each)
	})

	v, _ := url.ParseQuery("after=0&limit=500")
	req, _ := parseMessages(v)
	first, err := FetchPage(t.Context(), s, in, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(first.Messages); got != 2 {
		t.Fatalf("the first page held %d messages of 6 MiB, want 2 (budget 16 MiB)", got)
	}
	if first.Next == nil || !strings.Contains(*first.Next, "after=2") {
		t.Errorf("next = %v; the cut-off page must continue from the last message it holds", first.Next)
	}

	// Following next still visits every message exactly once.
	seqs, pages, _ := follow(t, s, in, "after=0&limit=500")
	if !sameSeqs(seqs, seqRange(1, 5, false)) || pages != 3 {
		t.Errorf("saw %v over %d pages, want 1..5 over 3", seqs, pages)
	}
	// Descending pages are cut the same way.
	desc, _, _ := follow(t, s, in, "limit=500")
	if !sameSeqs(desc, seqRange(1, 5, true)) {
		t.Errorf("descending saw %v", desc)
	}
}

func TestAMessageLargerThanTheBudgetStillFormsAPage(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 3, func(i int, m *inbox.NewMessage) {
		m.Body = bytes.Repeat([]byte("x"), 20<<20) // over the whole budget on its own
	})
	seqs, pages, byseq := follow(t, s, in, "after=0")
	if !sameSeqs(seqs, seqRange(1, 3, false)) || pages != 3 {
		t.Errorf("saw %v over %d pages", seqs, pages)
	}
	for seq, m := range byseq {
		if m.BodyText == nil || len(*m.BodyText) != 20<<20 {
			t.Errorf("seq %d lost its body", seq)
		}
	}
}

func TestSummaryIgnoresTheBodyBudget(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 5, func(i int, m *inbox.NewMessage) { m.Body = bytes.Repeat([]byte("x"), 6<<20) })
	v, _ := url.ParseQuery("after=0&fields=summary&limit=500")
	req, _ := parseMessages(v)
	page, err := FetchPage(t.Context(), s, in, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 5 {
		t.Errorf("a summary page held %d messages, want all 5", len(page.Messages))
	}
}

// failingGet makes loading one message's body fail the way a concurrent prune does.
type failingGet struct {
	inbox.Store
	seq int64
	err error
}

func (f failingGet) Get(ctx context.Context, id inbox.ID, seq int64) (*inbox.Message, error) {
	if seq == f.seq {
		return nil, f.err
	}
	return f.Store.Get(ctx, id, seq)
}

func TestAMessagePrunedMidPageIsLeftOut(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 3, func(i int, m *inbox.NewMessage) { m.Body = bytes.Repeat([]byte("y"), InlineBodyBytes+10) })
	st := failingGet{Store: s, seq: 2, err: inbox.ErrNotFound}
	v, _ := url.ParseQuery("after=0")
	req, _ := parseMessages(v)
	page, err := FetchPage(t.Context(), st, in, req)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, m := range page.Messages {
		got = append(got, m.Seq)
	}
	if !sameSeqs(got, []int64{1, 3}) {
		t.Errorf("seqs = %v, want [1 3]", got)
	}
	// Any other failure is an error, not a silently short page.
	st.err = fmt.Errorf("disk on fire")
	if _, err := FetchPage(t.Context(), st, in, req); err == nil {
		t.Error("a real failure was swallowed")
	}
}

func TestFetchInfo(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 4, nil)
	info, err := FetchInfo(t.Context(), s, in)
	if err != nil {
		t.Fatal(err)
	}
	if info.Slug != "wire-test" || info.Verifier != "github" || info.MessageCount != 4 || info.OldestSeq != 1 || info.LatestSeq != 4 {
		t.Errorf("info = %+v", info)
	}
	raw, _ := Marshal(info)
	for _, secret := range []string{"hash", "secret", "token"} {
		if strings.Contains(strings.ToLower(string(raw)), secret) {
			t.Errorf("the inbox info mentions %q: %s", secret, raw)
		}
	}
}

// querySpy records the queries that reach the Store.
type querySpy struct {
	inbox.Store
	queries []inbox.Query
	gets    int
}

func (q *querySpy) Query(ctx context.Context, id inbox.ID, query inbox.Query) (*inbox.Page, error) {
	q.queries = append(q.queries, query)
	return q.Store.Query(ctx, id, query)
}

func (q *querySpy) Get(ctx context.Context, id inbox.ID, seq int64) (*inbox.Message, error) {
	q.gets++
	return q.Store.Get(ctx, id, seq)
}

// The memory bound is a promise about what the Store is asked to load, so test
// the request itself: a page query must never ask for unbounded bodies.
func TestPageQueriesNeverAskForUnboundedBodies(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 3, func(i int, m *inbox.NewMessage) {
		m.Body = bytes.Repeat([]byte("z"), InlineBodyBytes*(i-1)+10) // small, just over the limit, well over it
	})

	spy := &querySpy{Store: s}
	v, _ := url.ParseQuery("after=0")
	req, _ := parseMessages(v)
	if _, err := FetchPage(t.Context(), spy, in, req); err != nil {
		t.Fatal(err)
	}
	if len(spy.queries) != 1 || spy.queries[0].BodyLimit == nil || *spy.queries[0].BodyLimit != InlineBodyBytes {
		t.Fatalf("the page query asked for %+v; it must cap bodies at %d bytes", spy.queries, InlineBodyBytes)
	}
	// Only the two oversized bodies were fetched one by one; the small one came
	// with the query.
	if spy.gets != 2 {
		t.Errorf("%d single-message loads; want 2 (the messages over %d bytes)", spy.gets, InlineBodyBytes)
	}

	// A summary asks for no bodies at all, and loads none.
	spy = &querySpy{Store: s}
	v, _ = url.ParseQuery("after=0&fields=summary")
	req, _ = parseMessages(v)
	if _, err := FetchPage(t.Context(), spy, in, req); err != nil {
		t.Fatal(err)
	}
	if spy.queries[0].BodyLimit == nil || *spy.queries[0].BodyLimit != 0 || spy.gets != 0 {
		t.Errorf("summary: BodyLimit %v, %d single loads; want 0 and none", spy.queries[0].BodyLimit, spy.gets)
	}
}

func fetch(t *testing.T, s inbox.Store, in *inbox.Inbox, raw string, budget int64) *Page {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	req, err := parseMessages(v)
	if err != nil {
		t.Fatal(err)
	}
	req.Budget = budget
	page, err := FetchPage(t.Context(), s, in, req)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestCursorSaysWhereAnAscendingReadContinues(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 100, func(i int, m *inbox.NewMessage) {
		if i == 5 || i == 10 {
			m.EventType = "rare"
		}
	})

	// A filtered read that matches only two early messages has seen everything
	// up to the latest seq, so the stream need not look at those rows again.
	p := fetch(t, s, in, "after=0&limit=50&type=rare", 0)
	if len(p.Messages) != 2 || !p.Complete || p.Cursor != 100 {
		t.Errorf("filtered read: %d messages, complete=%v, cursor=%d; want 2, true, 100", len(p.Messages), p.Complete, p.Cursor)
	}

	// A full page has not seen everything: continue from the last message.
	p = fetch(t, s, in, "after=0&limit=10", 0)
	if len(p.Messages) != 10 || p.Complete || p.Cursor != 10 {
		t.Errorf("full page: %d messages, complete=%v, cursor=%d; want 10, false, 10", len(p.Messages), p.Complete, p.Cursor)
	}

	// Reading from the middle to the end completes at the latest seq.
	p = fetch(t, s, in, "after=95&limit=50", 0)
	if len(p.Messages) != 5 || !p.Complete || p.Cursor != 100 {
		t.Errorf("tail: %d messages, complete=%v, cursor=%d", len(p.Messages), p.Complete, p.Cursor)
	}

	// Nothing new: complete, and the cursor does not move backward.
	p = fetch(t, s, in, "after=100", 0)
	if len(p.Messages) != 0 || !p.Complete || p.Cursor != 100 {
		t.Errorf("caught up: %d messages, complete=%v, cursor=%d", len(p.Messages), p.Complete, p.Cursor)
	}
	// A cursor ahead of everything stays where it is.
	p = fetch(t, s, in, "after=500", 0)
	if len(p.Messages) != 0 || !p.Complete || p.Cursor != 500 {
		t.Errorf("ahead of the inbox: complete=%v, cursor=%d; want true, 500", p.Complete, p.Cursor)
	}
}

func TestACutPageIsNeverComplete(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 4, func(i int, m *inbox.NewMessage) { m.Body = bytes.Repeat([]byte("x"), 300<<10) })
	// A tiny budget fits one message at a time even though the store returned all
	// four, which is fewer than the limit.
	p := fetch(t, s, in, "after=0&limit=50", 400<<10)
	if len(p.Messages) != 1 || p.Complete || p.Cursor != 1 {
		t.Errorf("%d messages, complete=%v, cursor=%d; want 1, false, 1", len(p.Messages), p.Complete, p.Cursor)
	}
	// Following the cursor reads everything exactly once.
	var seen []int64
	cursor := int64(0)
	for range 10 {
		p = fetch(t, s, in, fmt.Sprintf("after=%d&limit=50", cursor), 400<<10)
		for _, m := range p.Messages {
			seen = append(seen, m.Seq)
		}
		cursor = p.Cursor
		if p.Complete {
			break
		}
	}
	if !sameSeqs(seen, []int64{1, 2, 3, 4}) || cursor != 4 {
		t.Errorf("seen %v, final cursor %d", seen, cursor)
	}
}

func TestACursorAdvancesPastMessagesPrunedMidRead(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 3, func(i int, m *inbox.NewMessage) { m.Body = bytes.Repeat([]byte("y"), InlineBodyBytes+10) })
	st := failingGet{Store: s, seq: 2, err: inbox.ErrNotFound}
	v, _ := url.ParseQuery("after=0&limit=2") // a full page, so not complete
	req, _ := parseMessages(v)
	page, err := FetchPage(t.Context(), st, in, req)
	if err != nil {
		t.Fatal(err)
	}
	if page.Complete || page.Cursor != 2 {
		t.Errorf("complete=%v cursor=%d; want a cursor that moves past the pruned seq 2", page.Complete, page.Cursor)
	}
}

func TestDescendingReadsHaveNoCursor(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 5, nil)
	p := fetch(t, s, in, "limit=3", 0)
	if p.Complete || p.Cursor != 0 {
		t.Errorf("complete=%v cursor=%d; a newest-first read has neither", p.Complete, p.Cursor)
	}
}

func TestBodyCapLeavesOutBigBodiesAndSaysSo(t *testing.T) {
	s, in := newStore(t)
	add(t, s, in, 3, func(i int, m *inbox.NewMessage) {
		switch i {
		case 1:
			m.Body = []byte(`{"small":true}`)
		case 2:
			m.Body = bytes.Repeat([]byte("x"), 5000)
		case 3:
			m.Body = nil // an empty body is not a truncated one
		}
	})
	asc := int64(0)
	page, err := FetchPage(t.Context(), s, in, PageRequest{
		Query: inbox.Query{After: &asc, Limit: 10}, Limit: 10, BodyCap: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("got %d messages", len(page.Messages))
	}
	small, big, empty := page.Messages[0], page.Messages[1], page.Messages[2]
	if small.BodyTruncated || string(small.Body) != `{"small":true}` {
		t.Errorf("a small body must come through whole: %+v", small)
	}
	if !big.BodyTruncated || big.Body != nil || big.BodyText != nil || big.BodyBase64 != nil {
		t.Errorf("a big body must be left out and marked: %+v", big)
	}
	if big.Size != 5000 {
		t.Errorf("size must still be the true length, got %d", big.Size)
	}
	if empty.BodyTruncated {
		t.Errorf("an empty body is not truncated: %+v", empty)
	}

	// Without a cap nothing is ever marked, and the big body is loaded.
	page, err = FetchPage(t.Context(), s, in, PageRequest{Query: inbox.Query{After: &asc, Limit: 10}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Messages[1].BodyTruncated || page.Messages[1].BodyText == nil {
		t.Errorf("without a cap the body must be loaded: %+v", page.Messages[1])
	}
}
