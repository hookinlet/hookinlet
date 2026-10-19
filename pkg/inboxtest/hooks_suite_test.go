// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// ---- UnverifiedLimiter ----

type goodLimiter struct{ calls atomic.Int64 }

func (l *goodLimiter) MaxUnverified(_ context.Context, in *inbox.Inbox) int {
	l.calls.Add(1)
	if in.Tenant != "" {
		return 50
	}
	return 0
}

func TestUnverifiedLimiterSuite(t *testing.T) {
	UnverifiedLimiterSuite(t, func(*testing.T) inbox.UnverifiedLimiter { return &goodLimiter{} })
}

type blockingLimiter struct{ release chan struct{} }

func (l blockingLimiter) MaxUnverified(context.Context, *inbox.Inbox) int {
	<-l.release
	return 1
}

type rewritingLimiter struct{}

func (rewritingLimiter) MaxUnverified(_ context.Context, in *inbox.Inbox) int {
	in.Name += "!"
	return 1
}

type panickingLimiter struct{}

func (panickingLimiter) MaxUnverified(context.Context, *inbox.Inbox) int {
	panic("a bug in the limiter")
}

type cancelledLimiter struct{}

// It does a lookup that fails on a cancelled context, and waits for the caller's
// patience to run out.
func (cancelledLimiter) MaxUnverified(ctx context.Context, _ *inbox.Inbox) int {
	if ctx.Err() != nil {
		<-make(chan struct{})
	}
	return 1
}

func TestTheLimiterChecksCatchWhatTheyAreFor(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	for _, c := range []struct {
		name  string
		check func(testing.TB, inbox.UnverifiedLimiter)
		l     inbox.UnverifiedLimiter
		want  string
	}{
		{"a good limiter returns", checkLimiterReturns, &goodLimiter{}, ""},
		{"a good limiter leaves the inbox", checkLimiterLeavesTheInbox, &goodLimiter{}, ""},
		{"a good limiter is concurrent", checkLimiterConcurrent, &goodLimiter{}, ""},
		{"one that blocks", checkLimiterReturns, blockingLimiter{release}, "must not block"},
		{"one that waits on a cancelled context", checkLimiterReturns, cancelledLimiter{}, "a cancelled context did not return"},
		{"one that rewrites the inbox", checkLimiterLeavesTheInbox, rewritingLimiter{}, "changed the inbox"},
		{"one that panics", checkLimiterReturns, panickingLimiter{}, "panicked"},
	} {
		t.Run(c.name, func(t *testing.T) { expectFailure(t, c.want, func(rec testing.TB) { c.check(rec, c.l) }) })
	}
}

// expectFailure runs check against a recorder and requires that it reported want,
// or nothing if want is empty.
func expectFailure(t *testing.T, want string, check func(rec testing.TB)) {
	t.Helper()
	rec := &recordingTB{TB: t}
	check(rec)
	got := strings.Join(rec.errs, "\n")
	switch {
	case want == "" && got != "":
		t.Errorf("unexpected failure: %s", got)
	case want != "" && !strings.Contains(got, want):
		t.Errorf("failures %q do not mention %q", rec.errs, want)
	}
}

// ---- BodyScreener ----

type goodScreener struct{}

// It redacts digits, within the limit, and refuses empty bodies.
func (goodScreener) ScreenBody(_ context.Context, req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	if len(req.Body) == 0 {
		return inbox.BodyDecision{}, fmt.Errorf("%w: empty", inbox.ErrForbidden)
	}
	out := bytes.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'X'
		}
		return r
	}, req.Body)
	return inbox.BodyDecision{Replace: len(out) <= int(req.Inbox.MaxBodyBytes), Body: out}, nil
}

func TestBodyScreenerSuite(t *testing.T) {
	BodyScreenerSuite(t, func(*testing.T) inbox.BodyScreener { return goodScreener{} })
}

type blockingScreener struct{ release chan struct{} }

func (s blockingScreener) ScreenBody(context.Context, *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	<-s.release
	return inbox.BodyDecision{}, nil
}

type editingScreener struct{}

// It edits the body in place and says to keep it.
func (editingScreener) ScreenBody(_ context.Context, req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	if len(req.Body) > 0 {
		req.Body[0] = 'X'
	}
	return inbox.BodyDecision{}, nil
}

type growingScreener struct{}

// It replaces every body with one longer than any inbox allows.
func (growingScreener) ScreenBody(_ context.Context, req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	return inbox.BodyDecision{Replace: true, Body: make([]byte, req.Inbox.MaxBodyBytes+1)}, nil
}

type panickingScreener struct{}

func (panickingScreener) ScreenBody(context.Context, *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	panic("a bug in the screener")
}

func TestTheScreenerChecksCatchWhatTheyAreFor(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	for _, c := range []struct {
		name  string
		check func(testing.TB, inbox.BodyScreener)
		s     inbox.BodyScreener
		want  string
	}{
		{"a good screener returns", checkScreenerReturns, goodScreener{}, ""},
		{"a good screener leaves the body", checkScreenerLeavesTheBody, goodScreener{}, ""},
		{"a good screener's replacements fit", checkScreenerReplacementsFit, goodScreener{}, ""},
		{"a good screener is concurrent", checkScreenerConcurrent, goodScreener{}, ""},
		{"one that blocks", checkScreenerReturns, blockingScreener{release}, "must not block"},
		{"one that panics", checkScreenerReturns, panickingScreener{}, "panicked"},
		{"one that edits the body in place", checkScreenerLeavesTheBody, editingScreener{}, "changed the body"},
		{"one that grows the body past the limit", checkScreenerReplacementsFit, growingScreener{}, "over the inbox's MaxBodyBytes"},
	} {
		t.Run(c.name, func(t *testing.T) { expectFailure(t, c.want, func(rec testing.TB) { c.check(rec, c.s) }) })
	}
}

// ---- IngestObserver ----

type goodObserver struct{ seen atomic.Int64 }

func (o *goodObserver) ObserveIngest(_ context.Context, ev *inbox.IngestEvent) {
	o.seen.Add(1)
	_ = ev.Inbox.Slug + string(ev.Outcome)
}

func TestIngestObserverSuite(t *testing.T) {
	IngestObserverSuite(t, func(*testing.T) inbox.IngestObserver { return &goodObserver{} })
}

type blockingObserver struct{ release chan struct{} }

func (o blockingObserver) ObserveIngest(context.Context, *inbox.IngestEvent) { <-o.release }

type rewritingObserver struct{}

func (rewritingObserver) ObserveIngest(_ context.Context, ev *inbox.IngestEvent) {
	ev.Inbox.Slug = "other"
}

type clearingObserver struct{}

func (clearingObserver) ObserveIngest(_ context.Context, ev *inbox.IngestEvent) { ev.Size = 0 }

type surprisedObserver struct{}

// It does not expect an outcome it does not know.
func (surprisedObserver) ObserveIngest(_ context.Context, ev *inbox.IngestEvent) {
	if !slices.Contains(ingestOutcomes, ev.Outcome) {
		panic("unknown outcome " + string(ev.Outcome))
	}
}

func TestTheObserverChecksCatchWhatTheyAreFor(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	for _, c := range []struct {
		name  string
		check func(testing.TB, inbox.IngestObserver)
		o     inbox.IngestObserver
		want  string
	}{
		{"a good observer returns", checkObserverReturns, &goodObserver{}, ""},
		{"a good observer leaves the event", checkObserverLeavesTheEvent, &goodObserver{}, ""},
		{"a good observer is concurrent", checkObserverConcurrent, &goodObserver{}, ""},
		{"one that blocks", checkObserverReturns, blockingObserver{release}, "must not block"},
		{"one that rewrites the inbox", checkObserverLeavesTheEvent, rewritingObserver{}, "changed the event"},
		{"one that rewrites the event", checkObserverLeavesTheEvent, clearingObserver{}, "changed the event"},
		{"one that panics on a new outcome", checkObserverReturns, surprisedObserver{}, `"an_outcome_added_later"`},
	} {
		t.Run(c.name, func(t *testing.T) { expectFailure(t, c.want, func(rec testing.TB) { c.check(rec, c.o) }) })
	}
}

// ingestOutcomes lists every IngestOutcome that pkg/inbox declares.
func TestTheObserverSuiteCoversEveryOutcome(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "inbox", "interfaces.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			// A constant of type IngestOutcome is declared with that type, or follows one that was.
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "IngestOutcome" {
				for _, n := range vs.Names {
					declared[n.Name] = true
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no IngestOutcome constants; the scan is broken")
	}
	// The constants without their own type line (IngestQuotaExceeded and the like) are
	// listed with a type on every line in interfaces.go, which this scan relies on.
	if len(declared) != len(ingestOutcomes) {
		t.Errorf("pkg/inbox declares %d IngestOutcome constants and ingestOutcomes lists %d: add the new one to ingestOutcomes (hooks_suite.go)", len(declared), len(ingestOutcomes))
	}
}

// ---- Challenger ----

type goodChallenger struct{ value string }

func (c goodChallenger) Error() string           { return "unauthenticated" }
func (c goodChallenger) Unwrap() error           { return inbox.ErrUnauthenticated }
func (c goodChallenger) WWWAuthenticate() string { return c.value }

const goodChallenge = `Bearer resource_metadata="https://hooks.example.com/.well-known/oauth-protected-resource"`

func TestChallengerSuite(t *testing.T) {
	ChallengerSuite(t, func(*testing.T) inbox.Challenger { return goodChallenger{goodChallenge} })
}

// The values pkg/server's TestValidChallenge accepts and refuses (the server's
// challenge_test.go runs the good ones through ChallengerSuite).
func TestTheChallengeRulesAreTheServers(t *testing.T) {
	for _, v := range []string{
		"Bearer", `Bearer realm="hookinlet"`, `Bearer realm="x", error="invalid_token"`, "Negotiate",
		"X-Custom.scheme~1 a=b", "Bearer  two-spaces", strings.Repeat("a", 4096),
	} {
		if p := challengeProblem(v); p != "" {
			t.Errorf("%q refused: %s", v, p)
		}
	}
	for name, v := range map[string]string{
		"empty": "", "only a space": " ", "leading space": ` Bearer realm="x"`, "no scheme": `realm="x"`,
		"starts with a quote": `"Bearer" x`, "scheme then comma": `Bearer,realm="x"`, "scheme then equals": `Bearer=x`,
		"scheme then tab": "Bearer\trealm=x", "scheme with a slash": `Bea/rer x`, "LF": "Bearer realm=\"x\"\nSet-Cookie: a=b",
		"CRLF": "Bearer\r\nX-Injected: 1", "NUL": "Bearer \x00", "DEL": "Bearer \x7f", "tab inside": "Bearer a=\tb",
		"non-ASCII": "Bearer realm=\"café\"", "too long": strings.Repeat("a", 4097),
	} {
		if challengeProblem(v) == "" {
			t.Errorf("%s: %q accepted", name, v)
		}
	}
}

type plainChallenger struct{ value string }

func (c plainChallenger) WWWAuthenticate() string { return c.value }

type otherError struct{ goodChallenger }

func (otherError) Unwrap() error { return fmt.Errorf("something else") }

type driftingChallenger struct {
	goodChallenger
	n *atomic.Int64
}

func (c driftingChallenger) WWWAuthenticate() string {
	return fmt.Sprintf("Bearer realm=%d", c.n.Add(1))
}

func TestTheChallengerChecksCatchWhatTheyAreFor(t *testing.T) {
	for _, c := range []struct {
		name  string
		check func(testing.TB, inbox.Challenger)
		c     inbox.Challenger
		want  string
	}{
		{"a good challenge", checkChallengeValue, goodChallenger{goodChallenge}, ""},
		{"a good challenge is stable", checkChallengeStable, goodChallenger{goodChallenge}, ""},
		{"a good challenge is an unauthenticated error", checkChallengeError, goodChallenger{goodChallenge}, ""},
		{"not an error at all", checkChallengeError, plainChallenger{goodChallenge}, ""},
		{"two lines", checkChallengeValue, goodChallenger{"Bearer realm=\"x\"\nSet-Cookie: a=b"}, "not printable ASCII"},
		{"no scheme", checkChallengeValue, goodChallenger{`"Bearer" x`}, "does not start with an authentication scheme"},
		{"not a scheme", checkChallengeValue, goodChallenger{`realm="x"`}, "must end or be followed by a space"},
		{"empty", checkChallengeValue, goodChallenger{""}, "it is empty"},
		{"a value built from the request", checkChallengeStable, driftingChallenger{goodChallenger{goodChallenge}, new(atomic.Int64)}, "must not depend on the request"},
		{"an error that is not unauthenticated", checkChallengeError, otherError{goodChallenger{goodChallenge}}, "does not wrap inbox.ErrUnauthenticated"},
	} {
		t.Run(c.name, func(t *testing.T) { expectFailure(t, c.want, func(rec testing.TB) { c.check(rec, c.c) }) })
	}
}
