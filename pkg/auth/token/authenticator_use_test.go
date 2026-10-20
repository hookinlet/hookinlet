// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package token_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/inboxtest"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// plainStore hides everything a Store offers beyond inbox.Store, TouchToken included.
type plainStore struct{ inbox.Store }

// touchStore records the calls to TouchToken that the Authenticator makes, and can
// slow or fail them.
type touchStore struct {
	*sqlite.Store
	mu      sync.Mutex
	touched []inbox.TokenID
	ats     []time.Time
	err     error
	gate    chan struct{} // when set, TouchToken waits for it to be closed
	ctxErr  atomic.Value  // the context's error as TouchToken saw it, after the gate
	calls   chan struct{}
}

func newTouchStore(s *sqlite.Store) *touchStore {
	return &touchStore{Store: s, calls: make(chan struct{}, 100)}
}

func (s *touchStore) TouchToken(ctx context.Context, id inbox.TokenID, at time.Time) error {
	if s.gate != nil {
		<-s.gate
	}
	if err := ctx.Err(); err != nil {
		s.ctxErr.Store(err)
	}
	s.mu.Lock()
	s.touched = append(s.touched, id)
	s.ats = append(s.ats, at)
	err := s.err
	s.mu.Unlock()
	s.calls <- struct{}{}
	return err
}

func (s *touchStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.touched)
}

func waitCall(t *testing.T, s *touchStore) {
	t.Helper()
	select {
	case <-s.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("TouchToken was not called")
	}
}

// settle gives a call that must not happen a moment to show that it does.
func settle() { time.Sleep(50 * time.Millisecond) }

func TestAnExpiredTokenIsRefusedEvenByAStoreThatReturnsIt(t *testing.T) {
	f := newFixture(t, adminToken)
	// The store's clock stays where it was, so the store still returns the token;
	// only the authenticator's clock moves, as if the store did not enforce expiry.
	late := inboxtest.NewClock(f.clock.Now().Add(2 * time.Hour))
	a := token.NewAuthenticator(f.store, adminToken, token.WithClock(late.Now))
	if _, err := f.store.GetTokenByHash(t.Context(), token.Hash(f.tokenA)); err != nil {
		t.Fatalf("the store should still return the token: %v", err)
	}
	p, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA))
	if p != nil || !errors.Is(err, inbox.ErrUnauthenticated) {
		t.Errorf("principal %v, error %v; want ErrUnauthenticated", p, err)
	}
	if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenB)); err != nil {
		t.Errorf("a token that has not expired: %v", err)
	}
}

func TestATokenIsRefusedFromTheMomentItExpires(t *testing.T) {
	f := newFixture(t, adminToken)
	rec, _ := f.store.GetTokenByHash(t.Context(), token.Hash(f.tokenA))
	clk := inboxtest.NewClock(rec.ExpiresAt.Add(-time.Nanosecond))
	a := token.NewAuthenticator(f.store, adminToken, token.WithClock(clk.Now))
	// The store's clock is earlier, so it returns the token and the authenticator decides.
	if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
		t.Errorf("just before the expiry: %v", err)
	}
	clk.Set(*rec.ExpiresAt)
	if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); !errors.Is(err, inbox.ErrUnauthenticated) {
		t.Errorf("at the expiry: %v", err)
	}
}

func TestLastUseIsRecordedOncePerInterval(t *testing.T) {
	f := newFixture(t, adminToken)
	ts := newTouchStore(f.store)
	a := token.NewAuthenticator(ts, adminToken, token.WithClock(f.clock.Now), token.WithLastUsedInterval(10*time.Minute))
	use := func(tok string) {
		t.Helper()
		if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+tok)); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		use(f.tokenA)
	}
	waitCall(t, ts)
	use(f.tokenB) // another token is its own
	waitCall(t, ts)
	f.clock.Advance(9 * time.Minute)
	use(f.tokenA)
	settle()
	if n := ts.count(); n != 2 {
		t.Fatalf("%d recordings before the interval is up, want 2 (one per token)", n)
	}
	f.clock.Advance(2 * time.Minute)
	use(f.tokenA)
	waitCall(t, ts)
	if n := ts.count(); n != 3 {
		t.Errorf("%d recordings after the interval, want 3", n)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !ts.ats[2].Equal(f.clock.Now()) {
		t.Errorf("the use was recorded at %v, want the clock's %v", ts.ats[2], f.clock.Now())
	}
}

func TestLastUseTakesDefaultIntervalAndCanBeTurnedOff(t *testing.T) {
	f := newFixture(t, adminToken)
	off := newTouchStore(f.store)
	a := token.NewAuthenticator(off, adminToken, token.WithLastUsedInterval(-1))
	for range 3 {
		if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
			t.Fatal(err)
		}
	}
	settle()
	if n := off.count(); n != 0 {
		t.Errorf("%d recordings with recording turned off", n)
	}

	def := newTouchStore(f.store)
	a = token.NewAuthenticator(def, adminToken, token.WithClock(f.clock.Now))
	use := func() {
		if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
			t.Fatal(err)
		}
	}
	use()
	waitCall(t, def)
	f.clock.Advance(token.DefaultLastUsedInterval - time.Second)
	use()
	settle()
	if n := def.count(); n != 1 {
		t.Errorf("%d recordings inside the default interval, want 1", n)
	}
	f.clock.Advance(time.Second)
	use()
	waitCall(t, def)
}

func TestAStoreWithoutARecorderHasNoLastUseAndNoError(t *testing.T) {
	f := newFixture(t, adminToken)
	a := token.NewAuthenticator(plainStore{f.store}, adminToken, token.WithClock(f.clock.Now))
	for range 3 {
		if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
			t.Fatal(err)
		}
	}
	settle()
	got, err := f.store.GetTokenByHash(t.Context(), token.Hash(f.tokenA))
	if err != nil || got.LastUsedAt != nil {
		t.Errorf("last use = %v, %v; want none", got.LastUsedAt, err)
	}
}

func TestAFailedRecordingNeverFailsTheRequest(t *testing.T) {
	f := newFixture(t, adminToken)
	ts := newTouchStore(f.store)
	ts.err = errors.New("the writer is busy")
	a := token.NewAuthenticator(ts, adminToken, token.WithClock(f.clock.Now))
	p, err := a.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA))
	if err != nil || p == nil {
		t.Fatalf("a failing recorder failed the request: %v, %v", p, err)
	}
	waitCall(t, ts)
}

func TestARecordingDoesNotWaitForTheRequestAndSurvivesItsEnd(t *testing.T) {
	f := newFixture(t, adminToken)
	ts := newTouchStore(f.store)
	ts.gate = make(chan struct{})
	a := token.NewAuthenticator(ts, adminToken, token.WithClock(f.clock.Now))

	ctx, cancel := context.WithCancel(t.Context())
	r := req(t, "Authorization", "Bearer "+f.tokenA).WithContext(ctx)
	done := make(chan error, 1)
	go func() { _, err := a.Authenticate(r); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Authenticate waited for a slow recorder")
	}
	cancel() // the request is over while the recording is still waiting
	close(ts.gate)
	waitCall(t, ts)
	if err, _ := ts.ctxErr.Load().(error); err != nil {
		t.Errorf("the recording ran under a context that the request's end cancelled: %v", err)
	}
}

func TestStuckRecordingsAreBounded(t *testing.T) {
	f := newFixture(t, adminToken)
	ts := newTouchStore(f.store)
	ts.gate = make(chan struct{})
	a := token.NewAuthenticator(ts, adminToken, token.WithClock(f.clock.Now))
	// Many distinct tokens, each due, against a recorder that never answers.
	var toks []string
	for i := range 40 {
		tok, hash, err := token.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.CreateToken(t.Context(), &inbox.Token{Name: "n", Hash: hash, AllInboxes: true}); err != nil {
			t.Fatal(err, i)
		}
		toks = append(toks, tok)
	}
	for _, tok := range toks {
		if _, err := a.Authenticate(req(t, "Authorization", "Bearer "+tok)); err != nil {
			t.Fatal(err)
		}
	}
	close(ts.gate)
	settle()
	if n := ts.count(); n > 8 {
		t.Errorf("%d recordings were started against a stuck recorder, want at most 8 at once", n)
	}
}

func TestLastUseReachesTheDefaultStore(t *testing.T) {
	f := newFixture(t, adminToken)
	if _, err := f.auth.Authenticate(req(t, "Authorization", "Bearer "+f.tokenA)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := f.store.GetTokenByHash(t.Context(), token.Hash(f.tokenA))
		if err != nil {
			t.Fatal(err)
		}
		if got.LastUsedAt != nil {
			if d := got.LastUsedAt.Sub(f.clock.Now()).Abs(); d > time.Second {
				t.Errorf("last use is %v, the clock says %v", got.LastUsedAt, f.clock.Now())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the last use was never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
