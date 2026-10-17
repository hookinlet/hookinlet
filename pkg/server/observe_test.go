// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// obsHooks is Hooks that also implements inbox.IngestObserver. It records every
// event, and how many times AfterIngest had run when each arrived.
type obsHooks struct {
	*recHooks
	mu     sync.Mutex
	events []inbox.IngestEvent
	afters []int // len(recHooks.after()) when each event arrived
	boom   bool
}

func (o *obsHooks) ObserveIngest(_ context.Context, ev *inbox.IngestEvent) {
	n := len(o.after())
	o.mu.Lock()
	o.events = append(o.events, *ev)
	o.afters = append(o.afters, n)
	boom := o.boom
	o.mu.Unlock()
	if boom {
		panic("observer exploded")
	}
}

func (o *obsHooks) seen() []inbox.IngestEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]inbox.IngestEvent(nil), o.events...)
}

// observed is an env whose Hooks observe ingest.
func observed(t *testing.T, mods ...func(*Options)) (*env, *obsHooks) {
	t.Helper()
	obs := &obsHooks{recHooks: &recHooks{}}
	e := newEnv(t, append([]func(*Options){func(o *Options) { o.Hooks = obs }}, mods...)...)
	e.hooks = obs.recHooks
	return e, obs
}

// one checks that exactly one event was observed and returns it.
func one(t *testing.T, obs *obsHooks) inbox.IngestEvent {
	t.Helper()
	evs := obs.seen()
	if len(evs) != 1 {
		t.Fatalf("%d events observed, want 1: %+v", len(evs), evs)
	}
	return evs[0]
}

type gone struct{ inbox.Store }

func (gone) Append(context.Context, inbox.ID, *inbox.NewMessage) (*inbox.Message, bool, error) {
	return nil, false, inbox.ErrNotFound
}

type broken struct{ inbox.Store }

func (broken) Append(context.Context, inbox.ID, *inbox.NewMessage) (*inbox.Message, bool, error) {
	return nil, false, errors.New("the database went away")
}

// Every way a delivery to a known inbox ends is observed once, with its outcome,
// its status and the inbox it was for, tenant included.
func TestEveryDeliveryToAKnownInboxIsObservedOnce(t *testing.T) {
	tenant := func(in *inbox.Inbox) { in.Tenant = "acme" }
	slackSig := func(secret, body string) string {
		return "v0=" + hex.EncodeToString(mac256([]byte(secret), "v0:"+signedTS+":"+body))
	}
	hookErr := func(err error) func(*testing.T, *env) {
		return func(_ *testing.T, e *env) {
			e.hooks.before = func(*inbox.Inbox, int64) error { return err }
		}
	}
	unknownLength := int64(-1)

	cases := []struct {
		name     string
		verifier string
		secret   string
		inbox    func(*inbox.Inbox)
		opts     func(*Options)
		setup    func(*testing.T, *env) // runs before the delivery that is checked
		deliver  func(*env) *httptest.ResponseRecorder
		outcome  inbox.IngestOutcome
		status   int
		seq      bool // the event carries a seq
		verified bool
	}{
		{name: "stored", verifier: "trusted",
			outcome: inbox.IngestStored, status: 200, seq: true, verified: true},
		{name: "stored unverified", verifier: "none",
			outcome: inbox.IngestStored, status: 200, seq: true},
		{name: "stored after a failed signature", verifier: "github", secret: "s",
			inbox: func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore },
			deliver: func(e *env) *httptest.ResponseRecorder {
				return e.post("/in/box", "{}", "X-Hub-Signature-256", githubSig("wrong", "{}"))
			},
			outcome: inbox.IngestStored, status: 200, seq: true},
		{name: "duplicate", verifier: "github", secret: "s",
			setup: func(t *testing.T, e *env) {
				if rec := e.post("/in/box", "{}", "X-Hub-Signature-256", githubSig("s", "{}"), "X-GitHub-Delivery", "d1"); rec.Code != 200 {
					t.Fatalf("first delivery: %d", rec.Code)
				}
			},
			deliver: func(e *env) *httptest.ResponseRecorder {
				return e.post("/in/box", "{}", "X-Hub-Signature-256", githubSig("s", "{}"), "X-GitHub-Delivery", "d1")
			},
			outcome: inbox.IngestDuplicate, status: 200, seq: true, verified: true},
		{name: "handshake", verifier: "slack", secret: "s",
			deliver: func(e *env) *httptest.ResponseRecorder {
				body := `{"type":"url_verification","challenge":"c"}`
				return e.post("/in/box", body, "X-Slack-Request-Timestamp", signedTS, "X-Slack-Signature", slackSig("s", body))
			},
			outcome: inbox.IngestHandshake, status: 200},
		{name: "verification failed", verifier: "github", secret: "s",
			deliver: func(e *env) *httptest.ResponseRecorder {
				return e.post("/in/box", "{}", "X-Hub-Signature-256", githubSig("wrong", "{}"))
			},
			outcome: inbox.IngestVerificationFailed, status: 401},
		{name: "rate limited by the hook", verifier: "trusted", setup: hookErr(inbox.ErrRateLimited),
			outcome: inbox.IngestRateLimited, status: 429},
		{name: "quota exceeded by the hook", verifier: "trusted", setup: hookErr(inbox.ErrQuotaExceeded),
			outcome: inbox.IngestQuotaExceeded, status: 402},
		{name: "forbidden by the hook", verifier: "trusted", setup: hookErr(inbox.ErrForbidden),
			outcome: inbox.IngestForbidden, status: 403},
		{name: "the hook failed", verifier: "trusted", setup: hookErr(errors.New("hook broke")),
			outcome: inbox.IngestError, status: 500},
		{name: "unverified cap", verifier: "none", opts: func(o *Options) { o.MaxUnverified = 1 },
			setup: func(t *testing.T, e *env) {
				if rec := e.post("/in/box", "{}"); rec.Code != 200 {
					t.Fatalf("first delivery: %d", rec.Code)
				}
			},
			outcome: inbox.IngestUnverifiedLimit, status: 429},
		{name: "storage full", verifier: "trusted",
			setup:   func(_ *testing.T, e *env) { e.server.opts.Store = fullStore{e.store} },
			outcome: inbox.IngestStorageFull, status: 503},
		{name: "overloaded", verifier: "trusted",
			inbox: func(in *inbox.Inbox) { in.MaxBodyBytes = inbox.MaxBodyBytesCeiling },
			opts:  func(o *Options) { o.IngestMemoryBudget = inbox.MaxBodyBytesCeiling },
			setup: func(t *testing.T, e *env) {
				if !e.server.ingestBudget.add(1, noShare, inbox.MaxBodyBytesCeiling-1) {
					t.Fatal("could not hold the budget")
				}
			},
			outcome: inbox.IngestOverloaded, status: 503},
		{name: "too large by its Content-Length", verifier: "trusted",
			inbox:   func(in *inbox.Inbox) { in.MaxBodyBytes = 4 },
			deliver: func(e *env) *httptest.ResponseRecorder { return e.post("/in/box", "too long") },
			outcome: inbox.IngestTooLarge, status: 413},
		{name: "too large as it is read", verifier: "trusted",
			inbox: func(in *inbox.Inbox) { in.MaxBodyBytes = 4 },
			deliver: func(e *env) *httptest.ResponseRecorder {
				return e.do(req{path: "/in/box", body: "too long", length: &unknownLength})
			},
			outcome: inbox.IngestTooLarge, status: 413},
		{name: "unreadable body", verifier: "trusted",
			deliver: func(e *env) *httptest.ResponseRecorder {
				hr := httptest.NewRequestWithContext(e.t.Context(), http.MethodPost, "/in/box", io.NopCloser(errReader{}))
				rec := httptest.NewRecorder()
				e.server.Handler().ServeHTTP(rec, hr)
				return rec
			},
			outcome: inbox.IngestBadRequest, status: 400},
		{name: "disabled inbox", verifier: "trusted", inbox: func(in *inbox.Inbox) { in.Disabled = true },
			outcome: inbox.IngestNotFound, status: 404},
		{name: "inbox deleted during the delivery", verifier: "trusted",
			setup:   func(_ *testing.T, e *env) { e.server.opts.Store = gone{e.store} },
			outcome: inbox.IngestNotFound, status: 404},
		{name: "the store failed", verifier: "trusted",
			setup:   func(_ *testing.T, e *env) { e.server.opts.Store = broken{e.store} },
			outcome: inbox.IngestError, status: 500},
	}
	seen := map[inbox.IngestOutcome]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mods []func(*Options)
			if c.opts != nil {
				mods = append(mods, c.opts)
			}
			e, obs := observed(t, mods...)
			in := e.newInbox("box", c.verifier, c.secret, tenant, func(in *inbox.Inbox) {
				if c.inbox != nil {
					c.inbox(in)
				}
			})
			if c.setup != nil {
				c.setup(t, e)
			}
			obs.mu.Lock()
			obs.events, obs.afters = nil, nil
			obs.mu.Unlock()
			deliver := c.deliver
			if deliver == nil {
				deliver = func(e *env) *httptest.ResponseRecorder { return e.post("/in/box", "{}") }
			}
			rec := deliver(e)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body.String())
			}
			ev := one(t, obs)
			if ev.Outcome != c.outcome || ev.Status != c.status {
				t.Errorf("outcome %q status %d, want %q %d", ev.Outcome, ev.Status, c.outcome, c.status)
			}
			if ev.Inbox == nil || ev.Inbox.ID != in.ID || ev.Inbox.Slug != "box" || ev.Inbox.Tenant != "acme" || ev.Inbox.UID != in.UID {
				t.Errorf("the event names the inbox %+v, want %d box in acme", ev.Inbox, in.ID)
			}
			if c.seq != (ev.Seq > 0) {
				t.Errorf("seq %d", ev.Seq)
			}
			if c.seq && rec.Code == 200 && ev.Seq != seqOf(t, rec) {
				t.Errorf("seq %d, the sender was told %d", ev.Seq, seqOf(t, rec))
			}
			if ev.Verified != c.verified {
				t.Errorf("verified %v, want %v", ev.Verified, c.verified)
			}
			seen[ev.Outcome] = true
		})
	}
	// Each outcome the package declares is covered by some case above.
	for _, o := range []inbox.IngestOutcome{
		inbox.IngestStored, inbox.IngestDuplicate, inbox.IngestHandshake, inbox.IngestVerificationFailed,
		inbox.IngestRateLimited, inbox.IngestQuotaExceeded, inbox.IngestForbidden, inbox.IngestUnverifiedLimit,
		inbox.IngestStorageFull, inbox.IngestOverloaded, inbox.IngestTooLarge, inbox.IngestBadRequest,
		inbox.IngestNotFound, inbox.IngestError,
	} {
		if !seen[o] && !t.Failed() {
			t.Errorf("no case produced the outcome %q", o)
		}
	}
}

// The event says how big the delivery was and where it came from, and comes after
// AfterIngest has run.
func TestTheEventCarriesSizeAndSourceAndFollowsAfterIngest(t *testing.T) {
	e, obs := observed(t)
	e.newInbox("box", "trusted", "")
	if rec := e.do(req{path: "/in/box", body: `{"a":12345}`, remote: "203.0.113.7:4000"}); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	ev := one(t, obs)
	if ev.Size != int64(len(`{"a":12345}`)) || ev.SourceIP != "203.0.113.7" {
		t.Errorf("size %d source %q", ev.Size, ev.SourceIP)
	}
	obs.mu.Lock()
	afters := obs.afters
	obs.mu.Unlock()
	if afters[0] != 1 {
		t.Errorf("AfterIngest had run %d times when the event arrived, want 1", afters[0])
	}

	// A refused delivery reports what was read too.
	obs.mu.Lock()
	obs.events = nil
	obs.mu.Unlock()
	e.newInbox("strict", "github", "s")
	e.do(req{path: "/in/strict", body: "abcdef", remote: "198.51.100.1:1", headers: http.Header{"X-Hub-Signature-256": {githubSig("wrong", "abcdef")}}})
	if ev := one(t, obs); ev.Size != 6 || ev.SourceIP != "198.51.100.1" || ev.Outcome != inbox.IngestVerificationFailed {
		t.Errorf("refused: %+v", ev)
	}
}

// Nothing is observed when the key names no inbox, or the method is refused before
// the inbox is looked up: there is no inbox to say it was for.
func TestDeliveriesToNoInboxAreNotObserved(t *testing.T) {
	e, obs := observed(t)
	e.newInbox("real", "trusted", "")
	if rec := e.post("/in/guess", "{}"); rec.Code != 404 {
		t.Fatalf("unknown key: status %d", rec.Code)
	}
	if rec := e.do(req{method: http.MethodGet, path: "/in/real"}); rec.Code != 405 {
		t.Fatalf("wrong method: status %d", rec.Code)
	}
	if evs := obs.seen(); len(evs) != 0 {
		t.Errorf("observed %+v", evs)
	}
}

// The event carries nothing the sender sent: not the reason a signature failed,
// not a header, not the body.
func TestTheEventCarriesNothingTheSenderSent(t *testing.T) {
	e, obs := observed(t)
	e.newInbox("box", "github", "s")
	e.post("/in/box", "secret-body", "X-Hub-Signature-256", "sha256=00", "X-Private", "secret-header")
	ev := one(t, obs)
	for _, s := range []string{ev.SourceIP, string(ev.Outcome)} {
		for _, leak := range []string{"secret", "signature", "sha256"} {
			if strings.Contains(s, leak) {
				t.Errorf("the event carries %q: %+v", leak, ev)
			}
		}
	}
}

// A panic in the observer is logged; the delivery is still stored and acknowledged.
func TestAPanickingObserverDoesNotFailTheDelivery(t *testing.T) {
	e, obs := observed(t)
	obs.boom = true
	in := e.newInbox("box", "trusted", "")
	rec := e.post("/in/box", "{}")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if e.count(in) != 1 || len(e.hooks.after()) != 1 || len(e.notifier.published()) != 1 {
		t.Errorf("%d stored, %d AfterIngest, %d published", e.count(in), len(e.hooks.after()), len(e.notifier.published()))
	}
	if !strings.Contains(e.logs.String(), "panic in ingest observer") {
		t.Errorf("the panic was not logged: %s", e.logs.String())
	}
	// A refusal stays the refusal.
	e.newInbox("strict", "github", "s")
	if rec := e.post("/in/strict", "{}"); rec.Code != 401 {
		t.Errorf("a refused delivery with a panicking observer: status %d", rec.Code)
	}
}

// Hooks without the interface: the server finds no observer and ingest behaves as
// it always has.
func TestHooksWithoutAnObserverChangeNothing(t *testing.T) {
	e := newEnv(t)
	if e.server.observer != nil {
		t.Fatal("the server found an observer on Hooks that do not implement one")
	}
	in := e.newInbox("box", "trusted", "")
	if rec := e.post("/in/box", "{}"); rec.Code != 200 || e.count(in) != 1 || len(e.hooks.after()) != 1 {
		t.Errorf("status %d, %d stored, %d AfterIngest", rec.Code, e.count(in), len(e.hooks.after()))
	}
	e.newInbox("strict", "github", "s")
	if rec := e.post("/in/strict", "{}"); rec.Code != 401 {
		t.Errorf("status %d", rec.Code)
	}
}
