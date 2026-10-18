// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// hookWait is how long a hook that must not block is given to return.
const hookWait = 2 * time.Second

// This file holds the checks for the optional interfaces of Hooks and of an
// Authenticator's error that the server calls and nothing else reads: what they
// can be held to without a server is that they return at once, keep their hands off
// what they are given, and cope with being called from every request at once. What
// the server then does with the answers (the cap a limiter sets, the counts an
// observer makes) is the server's own tests.

// hookContexts are the contexts a hook is called with: an ordinary one, one that
// carries the request (AccessFrom), and one that is already cancelled.
func hookContexts(t testing.TB) map[string]context.Context { //nolint:revive // a table of contexts for the checks
	t.Helper()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	return map[string]context.Context{
		"a context":                 t.Context(),
		"a context with the access": inbox.WithAccess(t.Context(), inbox.AccessInfo{Operation: "ingest", RequestID: "req-1"}),
		"a cancelled context":       cancelled,
	}
}

// hookInboxes are inboxes of the kinds a hook meets: a plain one, and one with
// every field set.
func hookInboxes() []*inbox.Inbox {
	return []*inbox.Inbox{
		{ID: 1, Slug: "plain", Name: "plain", Verifier: "none"},
		{
			ID: 2, Slug: "full", Tenant: "some-tenant", UID: "uid-1234", IngestKey: "key-1234", Name: "full", Version: 3,
			Verifier: "hmac", VerifierConfig: []byte(`{"header":"X-Sig"}`), VerifierSecret: []byte("sealed"), OnVerifyFail: inbox.OnVerifyFailStore,
			MaxBodyBytes: 4096, RetentionDays: 7, RetentionMaxCount: 100, EventTypePointer: "/type", Epoch: "abc", LastSeq: 9, Disabled: true,
		},
	}
}

// cloneInbox copies an inbox, including what its slices point to.
func cloneInbox(in *inbox.Inbox) *inbox.Inbox {
	cp := *in
	cp.VerifierConfig = bytes.Clone(in.VerifierConfig)
	cp.VerifierSecret = bytes.Clone(in.VerifierSecret)
	return &cp
}

// within runs f and reports whether it returned in time, so that a check can stop
// at the first call that blocks instead of waiting for every one. A panic in f is
// reported as a failure and counts as returning.
func within(t testing.TB, what string, f func()) bool {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		f()
	}()
	select {
	case p := <-done:
		if p != nil {
			t.Errorf("%s panicked: %v", what, p)
		}
		return true
	case <-time.After(hookWait):
		t.Errorf("%s did not return in %v; it is called on every request and must not block", what, hookWait)
		return false
	}
}

// ---- UnverifiedLimiter ----

// UnverifiedLimiterSuite runs the checks that hold for every inbox.UnverifiedLimiter.
// newLimiter returns a limiter set up as the embedder uses it. The suite can only
// check what holds for every limiter: that MaxUnverified returns at once, with any
// context, does not change the inbox it is given, and is safe to call from many
// requests at once. Which cap it answers for which inbox is for the embedder's own
// tests.
func UnverifiedLimiterSuite(t *testing.T, newLimiter func(t *testing.T) inbox.UnverifiedLimiter) {
	t.Helper()
	for _, c := range []struct {
		name string
		fn   func(testing.TB, inbox.UnverifiedLimiter)
	}{
		{"ReturnsAtOnceWhateverTheContext", checkLimiterReturns},
		{"DoesNotChangeTheInbox", checkLimiterLeavesTheInbox},
		{"ConcurrentUseIsSafe", checkLimiterConcurrent},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLimiter(t)
			if l == nil {
				t.Fatal("newLimiter returned nil")
			}
			c.fn(t, l)
		})
	}
}

func checkLimiterReturns(t testing.TB, l inbox.UnverifiedLimiter) {
	t.Helper()
	for name, ctx := range hookContexts(t) {
		for _, in := range hookInboxes() {
			if !within(t, fmt.Sprintf("MaxUnverified for inbox %q with %s", in.Slug, name), func() { _ = l.MaxUnverified(ctx, in) }) {
				return
			}
		}
	}
}

func checkLimiterLeavesTheInbox(t testing.TB, l inbox.UnverifiedLimiter) {
	t.Helper()
	for _, in := range hookInboxes() {
		want := cloneInbox(in)
		if !within(t, "MaxUnverified", func() { _ = l.MaxUnverified(t.Context(), in) }) {
			return
		}
		if !reflect.DeepEqual(in, want) {
			t.Errorf("MaxUnverified changed the inbox it was given:\n got  %+v\n want %+v", in, want)
		}
	}
}

func checkLimiterConcurrent(t testing.TB, l inbox.UnverifiedLimiter) {
	t.Helper()
	_ = within(t, "concurrent calls to MaxUnverified", func() {
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				for i := range 200 {
					in := hookInboxes()[i%2]
					_ = l.MaxUnverified(t.Context(), in)
				}
			})
		}
		wg.Wait()
	})
}

// ---- IngestObserver ----

// ingestOutcomes is every IngestOutcome the package declares.
// TestTheObserverSuiteCoversEveryOutcome fails when one is missing.
var ingestOutcomes = []inbox.IngestOutcome{
	inbox.IngestStored, inbox.IngestDuplicate, inbox.IngestHandshake, inbox.IngestVerificationFailed,
	inbox.IngestRateLimited, inbox.IngestQuotaExceeded, inbox.IngestForbidden, inbox.IngestUnverifiedLimit,
	inbox.IngestStorageFull, inbox.IngestOverloaded, inbox.IngestTooLarge, inbox.IngestBadRequest,
	inbox.IngestNotFound, inbox.IngestError,
}

// ingestEvents returns an event of each outcome, and one of an outcome the package
// does not know, for the inboxes of hookInboxes.
func ingestEvents() []*inbox.IngestEvent {
	var out []*inbox.IngestEvent
	for i, outcome := range append(append([]inbox.IngestOutcome{}, ingestOutcomes...), "an_outcome_added_later") {
		ev := &inbox.IngestEvent{Inbox: hookInboxes()[i%2], Outcome: outcome, Status: 200, Size: int64(i), SourceIP: "192.0.2.1", Host: "hooks.example.com"}
		switch outcome {
		case inbox.IngestStored, inbox.IngestDuplicate:
			ev.Seq, ev.Verified = int64(i+1), true
		case inbox.IngestVerificationFailed:
			ev.Status, ev.VerifyFailure = 401, inbox.VerifyFailureMismatch
		}
		out = append(out, ev)
	}
	// An event with only what is always there.
	return append(out, &inbox.IngestEvent{Inbox: hookInboxes()[0], Outcome: inbox.IngestError, Status: 500})
}

// cloneEvent copies an event, with its inbox.
func cloneEvent(ev *inbox.IngestEvent) *inbox.IngestEvent {
	cp := *ev
	cp.Inbox = cloneInbox(ev.Inbox)
	return &cp
}

// IngestObserverSuite runs the checks that hold for every inbox.IngestObserver.
// newObserver returns an observer set up as the embedder uses it. ObserveIngest
// runs on the request's goroutine before the response is written, so the suite
// checks that it returns at once for every outcome (including one added later)
// whatever the context, does not change the event or its inbox, and is safe to call
// from many requests at once. What it does with the events is for the embedder's
// own tests.
func IngestObserverSuite(t *testing.T, newObserver func(t *testing.T) inbox.IngestObserver) {
	t.Helper()
	for _, c := range []struct {
		name string
		fn   func(testing.TB, inbox.IngestObserver)
	}{
		{"ReturnsAtOnceForEveryOutcomeAndContext", checkObserverReturns},
		{"DoesNotChangeTheEventOrItsInbox", checkObserverLeavesTheEvent},
		{"ConcurrentUseIsSafe", checkObserverConcurrent},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := newObserver(t)
			if o == nil {
				t.Fatal("newObserver returned nil")
			}
			c.fn(t, o)
		})
	}
}

func checkObserverReturns(t testing.TB, o inbox.IngestObserver) {
	t.Helper()
	for name, ctx := range hookContexts(t) {
		for _, ev := range ingestEvents() {
			if !within(t, fmt.Sprintf("ObserveIngest of %q with %s", ev.Outcome, name), func() { o.ObserveIngest(ctx, ev) }) {
				return
			}
		}
	}
}

func checkObserverLeavesTheEvent(t testing.TB, o inbox.IngestObserver) {
	t.Helper()
	for _, ev := range ingestEvents() {
		want := cloneEvent(ev)
		if !within(t, fmt.Sprintf("ObserveIngest of %q", ev.Outcome), func() { o.ObserveIngest(t.Context(), ev) }) {
			return
		}
		if !reflect.DeepEqual(ev, want) {
			t.Errorf("ObserveIngest changed the event it was given (outcome %q):\n got  %+v (inbox %+v)\n want %+v (inbox %+v)", ev.Outcome, ev, ev.Inbox, want, want.Inbox)
		}
	}
}

func checkObserverConcurrent(t testing.TB, o inbox.IngestObserver) {
	t.Helper()
	_ = within(t, "concurrent calls to ObserveIngest", func() {
		var wg sync.WaitGroup
		for g := range 16 {
			wg.Go(func() {
				events := ingestEvents()
				for i := range 200 {
					o.ObserveIngest(t.Context(), events[(g+i)%len(events)])
				}
			})
		}
		wg.Wait()
	})
}

// ---- Challenger ----

// ChallengerSuite runs the checks that hold for every inbox.Challenger. newChallenger
// returns the error an Authenticator gives for a request it refuses, as a Challenger.
// The server sends WWW-Authenticate only if the value is one it accepts (one line of
// printable ASCII, at most 4096 bytes, starting with an authentication scheme) and
// otherwise sends its own and logs a warning, so the suite fails a value the server
// would ignore. It also checks that the value is the same on every call, since
// it goes to every caller who is refused, and that the error wraps
// inbox.ErrUnauthenticated, without which the server does not look for a challenge.
func ChallengerSuite(t *testing.T, newChallenger func(t *testing.T) inbox.Challenger) {
	t.Helper()
	for _, c := range []struct {
		name string
		fn   func(testing.TB, inbox.Challenger)
	}{
		{"TheValueIsOneTheServerSends", checkChallengeValue},
		{"TheValueIsTheSameForEveryCaller", checkChallengeStable},
		{"TheErrorIsAnUnauthenticatedError", checkChallengeError},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := newChallenger(t)
			if ch == nil {
				t.Fatal("newChallenger returned nil")
			}
			c.fn(t, ch)
		})
	}
}

func checkChallengeValue(t testing.TB, c inbox.Challenger) {
	t.Helper()
	if p := challengeProblem(c.WWWAuthenticate()); p != "" {
		t.Errorf("WWWAuthenticate returned %q: %s; the server ignores it and sends its own challenge", c.WWWAuthenticate(), p)
	}
}

func checkChallengeStable(t testing.TB, c inbox.Challenger) {
	t.Helper()
	first := c.WWWAuthenticate()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if got := c.WWWAuthenticate(); got != first {
					mu.Lock()
					t.Errorf("WWWAuthenticate returned %q, then %q; it goes to every caller who is refused, so it must not depend on the request", first, got)
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
}

func checkChallengeError(t testing.TB, c inbox.Challenger) {
	t.Helper()
	err, ok := c.(error)
	if !ok {
		t.Logf("%T is not an error, so the check that it wraps inbox.ErrUnauthenticated is left to the Authenticator's own test", c)
		return
	}
	if !errors.Is(err, inbox.ErrUnauthenticated) {
		t.Errorf("%T does not wrap inbox.ErrUnauthenticated: the server looks for a challenge only on an error that does, and answers any other error with a 500", c)
	}
}

// challengeProblem says why v is not a WWW-Authenticate value the server sends, or
// returns "". The rules are the ones in docs/extending.md and inbox.Challenger.
func challengeProblem(v string) string {
	switch {
	case v == "":
		return "it is empty"
	case len(v) > 4096:
		return fmt.Sprintf("it is %d bytes, over the 4096 allowed", len(v))
	}
	for i := 0; i < len(v); i++ {
		if v[i] < ' ' || v[i] > '~' {
			return fmt.Sprintf("byte %d is not printable ASCII (no control character, tab included, and one line)", i)
		}
	}
	n := 0
	for n < len(v) && isChallengeTokenChar(v[n]) {
		n++
	}
	switch {
	case n == 0:
		return "it does not start with an authentication scheme such as Bearer"
	case n < len(v) && v[n] != ' ':
		return fmt.Sprintf("the scheme %q is followed by %q; it must end or be followed by a space and the parameters", v[:n], v[n:n+1])
	}
	return ""
}

// isChallengeTokenChar reports whether c is a tchar of RFC 9110.
func isChallengeTokenChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
