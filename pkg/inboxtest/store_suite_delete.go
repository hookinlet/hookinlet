// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// testDeleteRacesWrites deletes an inbox while appends and token issues for it are
// running. Whatever started before DeleteInbox returned may have succeeded or not,
// but once it has returned, no Append or CreateToken for the inbox succeeds (both
// are ErrNotFound), and nothing written to it during the race is readable: no
// message, and no token that still reads it.
func testDeleteRacesWrites(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	doomed := createInbox(t, s, "racing-doomed")
	other := createInbox(t, s, "racing-other")
	// Enough messages that a Store which deletes in batches is still at it while
	// the writers run.
	appendN(t, s, doomed.ID, 600, nil)

	var (
		deleted  atomic.Bool // set once DeleteInbox has returned
		mu       sync.Mutex
		problems []string
		seqs     []int64  // of messages Append said it stored in the doomed inbox
		hashes   []string // labels of the tokens CreateToken said it stored
	)
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(problems) < 10 {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}

	// Each writer keeps going until it has been refused after the delete returned,
	// a few times over, so that some of its calls certainly start afterwards.
	const appenders, issuers, afterwards = 4, 2, 3
	var wg sync.WaitGroup
	for g := range appenders {
		wg.Go(func() {
			for i, refused := 0, 0; refused < afterwards; i++ {
				after := deleted.Load()
				m, _, err := s.Append(ctx, doomed.ID, nm(func(m *inbox.NewMessage) { m.Body = fmt.Appendf(nil, "race-%d-%d", g, i) }))
				switch {
				case err == nil && after:
					fail("Append started after DeleteInbox returned stored seq %d", m.Seq)
					return
				case err == nil:
					mu.Lock()
					seqs = append(seqs, m.Seq)
					mu.Unlock()
				case !errors.Is(err, inbox.ErrNotFound):
					fail("Append during the delete: %v; want success or ErrNotFound", err)
					return
				case after:
					refused++
				}
			}
		})
	}
	for g := range issuers {
		wg.Go(func() {
			for i, refused := 0, 0; refused < afterwards; i++ {
				label := fmt.Sprintf("race-token-%d-%d", g, i)
				ids := []inbox.ID{doomed.ID}
				if i%2 == 1 {
					ids = append(ids, other.ID) // one that keeps another inbox
				}
				after := deleted.Load()
				err := s.CreateToken(ctx, mkToken(label, scoped(ids...)))
				switch {
				case err == nil && after:
					fail("CreateToken started after DeleteInbox returned stored a token for the deleted inbox")
					return
				case err == nil:
					mu.Lock()
					hashes = append(hashes, label)
					mu.Unlock()
				case !errors.Is(err, inbox.ErrNotFound):
					fail("CreateToken during the delete: %v; want success or ErrNotFound", err)
					return
				case after:
					refused++
				}
			}
		})
	}

	time.Sleep(5 * time.Millisecond) // let the writers get going
	if err := s.DeleteInbox(ctx, doomed.ID); err != nil {
		t.Errorf("DeleteInbox: %v", err)
	}
	deleted.Store(true)
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Minute):
		t.Fatal("writers were still succeeding a minute after DeleteInbox returned")
	}
	for _, p := range problems {
		t.Error(p)
	}

	// Nothing of the inbox is readable.
	_, err := s.GetInbox(ctx, doomed.ID)
	isErr(t, "GetInbox after the race", err, inbox.ErrNotFound)
	_, err = s.Query(ctx, doomed.ID, inbox.Query{})
	isErr(t, "Query after the race", err, inbox.ErrNotFound)
	_, err = s.Stats(ctx, doomed.ID)
	isErr(t, "Stats after the race", err, inbox.ErrNotFound)
	for _, seq := range append(seqRange(1, 600), seqs...) {
		if _, err := s.Get(ctx, doomed.ID, seq); !errors.Is(err, inbox.ErrNotFound) {
			t.Errorf("message %d of the deleted inbox: error %v, want ErrNotFound", seq, err)
			break
		}
	}
	// A token issued during the race either went with the inbox, or reads only the
	// other inbox it was also given.
	for _, label := range hashes {
		got, err := s.GetTokenByHash(ctx, tokenHash(label))
		switch {
		case errors.Is(err, inbox.ErrNotFound):
		case err != nil:
			t.Errorf("token %s: %v", label, err)
		case got.AllInboxes || !slices.Equal(got.InboxIDs, []inbox.ID{other.ID}):
			t.Errorf("token %s issued during the delete: %+v; want it gone, or reading only the other inbox", label, got)
		}
	}
	listed, err := s.ListTokens(ctx, inbox.TokenListParams{InboxID: doomed.ID, Limit: inbox.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Errorf("ListTokens for the deleted inbox lists %d tokens, the first %+v; want none", len(listed), listed[0])
	}

	// The other inbox is untouched.
	if st, err := s.Stats(ctx, other.ID); err != nil || st.MessageCount != 0 {
		t.Errorf("the other inbox after the race: %+v, %v", st, err)
	}
	if _, _, err := s.Append(ctx, other.ID, nm()); err != nil {
		t.Errorf("Append to the other inbox after the race: %v", err)
	}
}
