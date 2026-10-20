// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// screenHooks is Hooks that observe ingest and screen bodies with fn. A nil fn keeps
// every body.
type screenHooks struct {
	*obsHooks
	mu    sync.Mutex
	fn    func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error)
	calls []inbox.BodyScreenRequest
	// server, when set, is asked how much of the ingest budget is held when AfterIngest
	// runs (after the commit, before the delivery returns), and the answer is kept in
	// heldAtAfter.
	server      *Server
	heldAtAfter []int64
}

func (h *screenHooks) AfterIngest(ctx context.Context, in *inbox.Inbox, m *inbox.Message, created bool) {
	h.mu.Lock()
	if h.server != nil {
		h.server.ingestBudget.mu.Lock()
		h.heldAtAfter = append(h.heldAtAfter, h.server.ingestBudget.used)
		h.server.ingestBudget.mu.Unlock()
	}
	h.mu.Unlock()
	h.recHooks.AfterIngest(ctx, in, m, created)
}

func (h *screenHooks) ScreenBody(_ context.Context, req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	h.mu.Lock()
	h.calls = append(h.calls, inbox.BodyScreenRequest{Inbox: req.Inbox, Verified: req.Verified, Body: append([]byte(nil), req.Body...)})
	fn := h.fn
	h.mu.Unlock()
	if fn == nil {
		return inbox.BodyDecision{}, nil
	}
	return fn(req)
}

func (h *screenHooks) seenCalls() []inbox.BodyScreenRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]inbox.BodyScreenRequest(nil), h.calls...)
}

// screening is an env with the read API on, whose Hooks screen bodies with fn, and a
// token that reads the inbox "box" (verifier "trusted").
func screening(t *testing.T, fn func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error), mods ...func(*Options)) (*env, *screenHooks, *inbox.Inbox, string) {
	t.Helper()
	h := &screenHooks{obsHooks: &obsHooks{recHooks: &recHooks{}}, fn: fn}
	e := newEnv(t, append([]func(*Options){withAuth, func(o *Options) { o.Hooks = h }}, mods...)...)
	e.hooks = h.recHooks
	h.server = e.server
	in := e.newInbox("box", "trusted", "")
	tok, hash, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateToken(t.Context(), &inbox.Token{Name: "box", Hash: hash, InboxIDs: []inbox.ID{in.ID}}); err != nil {
		t.Fatal(err)
	}
	return e, h, in, tok
}

func replaceWith(body string) func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	return func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
		return inbox.BodyDecision{Replace: true, Body: []byte(body)}, nil
	}
}

func refuseWith(err error) func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
	return func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error) { return inbox.BodyDecision{}, err }
}

// A replaced body is stored, the message says so, and every way of reading it says so
// too: the read API, the raw endpoint, and the message that AfterIngest is given. The
// delivery stays verified, because the sender did send it.
func TestAReplacedBodyIsStoredAndMarkedModified(t *testing.T) {
	e, h, in, tok := screening(t, replaceWith(`{"card":"[redacted]"}`))
	rec := e.post("/in/box", `{"card":"4111111111111111"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delivery: %d %s", rec.Code, rec.Body)
	}
	msgs := e.messages(in)
	if len(msgs) != 1 || string(msgs[0].Body) != `{"card":"[redacted]"}` || !msgs[0].Modified || !msgs[0].Verified || msgs[0].Size != 21 {
		t.Fatalf("stored: %+v", msgs)
	}
	if got := h.after(); len(got) != 1 {
		t.Fatalf("AfterIngest calls: %v", got)
	}

	var page map[string]any
	if err := json.Unmarshal(e.get("/v1/inboxes/box/messages", tok).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	m := page["messages"].([]any)[0].(map[string]any)
	if m["modified"] != true || m["verified"] != true || fmt.Sprint(m["body"]) != "map[card:[redacted]]" {
		t.Errorf("read API message: %v", m)
	}
	raw := e.get("/v1/inboxes/box/messages/1/raw", tok)
	if raw.Code != http.StatusOK || raw.Body.String() != `{"card":"[redacted]"}` {
		t.Errorf("raw: %d %q", raw.Code, raw.Body)
	}
	if strings.Contains(e.logs.String(), "4111") {
		t.Error("the log holds the original body")
	}
	if ev := one(t, h.obsHooks); ev.Outcome != inbox.IngestStored || ev.Size != 27 {
		t.Errorf("observed %+v: Size is what was read, not what is stored", ev)
	}
}

// With no screener, or one that keeps every body, nothing about a delivery changes:
// the stored message, the responses, and the JSON the API gives are the same bytes, and
// a message has no "modified" field at all.
func TestWithoutAScreenerNothingChanges(t *testing.T) {
	received := regexp.MustCompile(`"(received_at|epoch)":"[^"]*",?`)
	run := func(t *testing.T, e *env, in *inbox.Inbox, tok string) (string, string, *inbox.Message) {
		rec := e.post("/in/box", `{"a":1}`, "Content-Type", "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery: %d %s", rec.Code, rec.Body)
		}
		listing := e.get("/v1/inboxes/box/messages", tok).Body.String()
		return rec.Body.String(), received.ReplaceAllString(listing, ""), e.messages(in)[0]
	}

	base := newEnv(t, withAuth)
	bin := base.newInbox("box", "trusted", "")
	btok, bhash, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := base.store.CreateToken(t.Context(), &inbox.Token{Name: "box", Hash: bhash, InboxIDs: []inbox.ID{bin.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(base.hooks).(inbox.BodyScreener); ok {
		t.Fatal("the default test hooks must not screen bodies")
	}
	baseAck, baseList, baseMsg := run(t, base, bin, btok)

	e, h, in, tok := screening(t, nil)
	ack, list, msg := run(t, e, in, tok)
	if ack != baseAck || list != baseList {
		t.Errorf("a screener that keeps the body changed a response:\n ack  %s vs %s\n list %s vs %s", ack, baseAck, list, baseList)
	}
	if strings.Contains(list, "modified") || msg.Modified || baseMsg.Modified {
		t.Errorf("a message that was not replaced says it is modified: %s", list)
	}
	if string(msg.Body) != string(baseMsg.Body) || msg.Size != baseMsg.Size || msg.Verified != baseMsg.Verified {
		t.Errorf("stored differently: %+v vs %+v", msg, baseMsg)
	}
	if calls := h.seenCalls(); len(calls) != 1 || !calls[0].Verified || string(calls[0].Body) != `{"a":1}` || calls[0].Inbox.Slug != "box" {
		t.Errorf("screener calls: %+v", calls)
	}
}

// A screener that returns the bytes it was given as a replacement has not modified
// anything, and the message does not say so.
func TestAReplacementEqualToTheOriginalIsNotAModification(t *testing.T) {
	e, _, in, _ := screening(t, replaceWith(`same`))
	if rec := e.post("/in/box", `same`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if m := e.messages(in)[0]; m.Modified || string(m.Body) != "same" {
		t.Errorf("%+v", m)
	}
}

// An empty replacement stores an empty body, which is a replacement like any other.
func TestAnEmptyReplacementStoresAnEmptyBody(t *testing.T) {
	for name, body := range map[string][]byte{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			e, _, in, _ := screening(t, func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
				return inbox.BodyDecision{Replace: true, Body: body}, nil
			})
			if rec := e.post("/in/box", `secret`); rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if m := e.messages(in)[0]; !m.Modified || m.Body == nil || len(m.Body) != 0 || m.Size != 0 {
				t.Errorf("%+v", m)
			}
		})
	}
}

// Without Replace the body of the decision is ignored.
func TestABodyWithoutReplaceIsIgnored(t *testing.T) {
	e, _, in, _ := screening(t, func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
		return inbox.BodyDecision{Body: []byte("ignored")}, nil
	})
	e.post("/in/box", `kept`)
	if m := e.messages(in)[0]; m.Modified || string(m.Body) != "kept" {
		t.Errorf("%+v", m)
	}
}

// A refusal is answered as BeforeIngest's is, stores nothing, and is reported to an
// observer as the body screener's, whatever its status. Other errors are internal
// failures, which an observer sees as errors and the sender does not see the cause of.
func TestARefusalIsMappedLikeBeforeIngest(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		status  int
		code    string
		outcome inbox.IngestOutcome
	}{
		{"rate limited", inbox.ErrRateLimited, 429, "rate_limited", inbox.IngestBodyRefused},
		{"quota", fmt.Errorf("plan: %w", inbox.ErrQuotaExceeded), 402, "quota_exceeded", inbox.IngestBodyRefused},
		{"forbidden", inbox.ErrForbidden, 403, "forbidden", inbox.IngestBodyRefused},
		{"not found", inbox.ErrNotFound, 404, "not_found", inbox.IngestBodyRefused},
		{"other", errors.New("the schema service is down"), 500, "internal", inbox.IngestError},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, h, in, _ := screening(t, refuseWith(c.err))
			rec := e.post("/in/box", `{"a":1}`)
			if rec.Code != c.status || errorCode(t, rec) != c.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "schema service") {
				t.Errorf("the cause reached the sender: %s", rec.Body)
			}
			if n := e.count(in); n != 0 {
				t.Errorf("%d messages stored", n)
			}
			if len(h.after()) != 0 {
				t.Error("AfterIngest ran for a refused delivery")
			}
			ev := one(t, h.obsHooks)
			if ev.Outcome != c.outcome || ev.Status != c.status || ev.Seq != 0 || ev.Verified {
				t.Errorf("observed %+v, want %s", ev, c.outcome)
			}
		})
	}
}

func TestARefusalCanSayWhenToRetry(t *testing.T) {
	e, _, _, _ := screening(t, refuseWith(inbox.RefuseAfter(inbox.ErrRateLimited, 7*time.Second)))
	rec := e.post("/in/box", `{}`)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "7" {
		t.Errorf("%d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// The screener only sees what is about to be stored. A delivery refused for its
// signature, a handshake, and one BeforeIngest refused never reach it; one kept
// unverified, and one to an inbox whose verifier checks nothing, do, and are told so.
func TestTheScreenerSeesOnlyWhatIsAboutToBeStored(t *testing.T) {
	e, h, _, _ := screening(t, nil)
	e.newInbox("ghbox", "github", "s")
	e.newInbox("keptbox", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	e.newInbox("openbox", "none", "")
	e.newInbox("slackbox", "slack", "slack-secret")

	if rec := e.post("/in/ghbox", `{}`, "X-Hub-Signature-256", githubSig("wrong", `{}`)); rec.Code != 401 {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	handshake := `{"type":"url_verification","challenge":"abc"}`
	if rec := e.post("/in/slackbox", handshake, "X-Slack-Request-Timestamp", signedTS,
		"X-Slack-Signature", "v0="+hexMAC("slack-secret", "v0:"+signedTS+":"+handshake)); rec.Code != 200 || rec.Body.String() != "abc" {
		t.Fatalf("handshake: %d %s", rec.Code, rec.Body)
	}
	e.hooks.before = func(*inbox.Inbox, int64) error { return inbox.ErrForbidden }
	if rec := e.post("/in/box", `{}`); rec.Code != 403 {
		t.Fatalf("BeforeIngest refusal: %d", rec.Code)
	}
	e.hooks.before = nil
	if n := len(h.seenCalls()); n != 0 {
		t.Fatalf("the screener was asked about %d deliveries that were not going to be stored", n)
	}

	for path, want := range map[string]bool{
		"/in/ghbox":   true,
		"/in/keptbox": false,
		"/in/openbox": false,
	} {
		sig := []string{}
		switch path {
		case "/in/ghbox":
			sig = []string{"X-Hub-Signature-256", githubSig("s", `{"n":1}`)}
		case "/in/keptbox":
			sig = []string{"X-Hub-Signature-256", githubSig("wrong", `{"n":1}`)}
		}
		before := len(h.seenCalls())
		if rec := e.post(path, `{"n":1}`, sig...); rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		calls := h.seenCalls()
		if len(calls) != before+1 || calls[before].Verified != want {
			t.Errorf("%s: calls %+v, want one with Verified=%v", path, calls[before:], want)
		}
	}
}

// A repeat of a delivery is recognised by what the sender sent. Where the delivery id
// is not covered by the signature the key includes a hash of the body, and that is of
// the original: the same body again is a duplicate however the screener rewrites it,
// and a different body under the same id is not one, even if both are rewritten into
// the same bytes.
func TestTheDedupeKeyComesFromTheOriginalBody(t *testing.T) {
	n := 0
	e, _, _, _ := screening(t, func(*inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
		n++ // not deterministic: every call makes something different
		return inbox.BodyDecision{Replace: true, Body: []byte(fmt.Sprintf(`{"redacted":%d}`, n))}, nil
	})
	gh := e.newInbox("ghbox", "github", "s")
	send := func(body string) int64 {
		t.Helper()
		rec := e.post("/in/ghbox", body, "X-Hub-Signature-256", githubSig("s", body), "X-GitHub-Delivery", "d1")
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return seqOf(t, rec)
	}
	first := send(`{"card":"1"}`)
	if again := send(`{"card":"1"}`); again != first {
		t.Errorf("a redelivery of the same body got seq %d, want %d", again, first)
	}
	if other := send(`{"card":"2"}`); other == first {
		t.Errorf("a different body under the same unsigned id collapsed into the first")
	}
	if msgs := e.messages(gh); len(msgs) != 2 || !msgs[0].Modified {
		t.Errorf("%d messages: %+v", len(msgs), msgs)
	}
}

// Where the signature covers the delivery id, a repeat is a duplicate whatever the
// body, as it is without a screener; the replacement plays no part.
func TestARepeatIsADuplicateWhenTheIDIsSigned(t *testing.T) {
	e, _, _, _ := screening(t, replaceWith(`{"x":1}`), func(o *Options) {
		if err := o.Verifiers.Register("signedid", func(json.RawMessage) (verify.Verifier, error) { return signedID{}, nil }); err != nil {
			t.Fatal(err)
		}
	})
	sid := e.newInbox("sidbox", "signedid", "")
	a, b := e.post("/in/sidbox", `{"a":1}`), e.post("/in/sidbox", `{"a":2}`)
	if a.Code != 200 || b.Code != 200 || seqOf(t, a) != seqOf(t, b) || e.count(sid) != 1 {
		t.Errorf("not collapsed: %s %s, %d stored", a.Body, b.Body, e.count(sid))
	}
}

// signedID is a verifier whose delivery id is covered by the signature.
type signedID struct{}

func (signedID) Verify(*http.Request, []byte, []byte, time.Time) (verify.Result, error) {
	return verify.Result{DeliveryID: "sid-1", IDSigned: true}, nil
}

// The event type pointer reads the body that is stored, so nothing is kept in a
// message's fields that the screener took out of its body.
func TestTheEventTypePointerReadsTheStoredBody(t *testing.T) {
	e, _, _, _ := screening(t, replaceWith(`{"type":"order.redacted","email":"x"}`))
	in := e.newInbox("typed", "trusted", "", func(in *inbox.Inbox) { in.EventTypePointer = "/type" })
	e.post("/in/typed", `{"type":"order.created","email":"a@example.com"}`)
	if m := e.messages(in)[0]; m.EventType != "order.redacted" {
		t.Errorf("event type %q, want the one in the stored body", m.EventType)
	}
}

// MaxBodyBytes bounds the replacement as it bounds the body: one that is longer is
// not truncated, and is an internal failure that stores nothing; one of exactly the
// limit is fine.
func TestAReplacementIsCappedAtTheInboxLimit(t *testing.T) {
	for _, c := range []struct {
		name   string
		size   int
		status int
	}{{"at the limit", 100, 200}, {"over the limit", 101, 500}} {
		t.Run(c.name, func(t *testing.T) {
			e, h, _, _ := screening(t, replaceWith(strings.Repeat("r", c.size)))
			in := e.newInbox("small", "trusted", "", func(in *inbox.Inbox) { in.MaxBodyBytes = 100 })
			rec := e.post("/in/small", `{"small":1}`)
			if rec.Code != c.status {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if got := e.count(in); (c.status == 200) != (got == 1) {
				t.Errorf("%d messages stored", got)
			}
			if c.status == 500 {
				if ev := one(t, h.obsHooks); ev.Outcome != inbox.IngestError {
					t.Errorf("observed %+v", ev)
				}
				if !strings.Contains(e.logs.String(), "over the inbox's limit") {
					t.Errorf("the cause is not logged: %s", e.logs.String())
				}
			}
		})
	}
}

// A replacement is held in memory beside the original, so it takes bytes from the
// ingest budget while the delivery is in flight and gives them back; one the budget
// has no room for is told to come back, as a body would be.
func TestAReplacementCountsAgainstTheIngestBudget(t *testing.T) {
	e, h, in, _ := screening(t, replaceWith(strings.Repeat("r", 300)),
		func(o *Options) { o.IngestMemoryBudget = inbox.MaxBodyBytesCeiling })
	if rec := e.post("/in/box", strings.Repeat("o", 200)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := h.heldAtAfter; len(got) != 1 || got[0] != 500 {
		t.Errorf("held while the delivery was stored: %v, want the original (200) and the replacement (300)", got)
	}
	if used := e.server.ingestBudget.used; used != 0 {
		t.Errorf("%d bytes of the budget are still held", used)
	}

	// Room for the original (200) but not for it and the replacement (500).
	room := e.server.ingestBudget.limit - 250
	if !e.server.ingestBudget.add(99, noShare, room) {
		t.Fatal("could not take the budget")
	}
	rec := e.post("/in/box", strings.Repeat("o", 200))
	if rec.Code != 503 || errorCode(t, rec) != "overloaded" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("no room for the replacement: %d %s", rec.Code, rec.Body)
	}
	if e.count(in) != 1 {
		t.Errorf("%d messages stored: a delivery refused for want of room was kept", e.count(in))
	}
	if used := e.server.ingestBudget.used; used != room {
		t.Errorf("the refused delivery kept %d bytes", used-room)
	}
	evs := h.seen()
	if last := evs[len(evs)-1]; last.Outcome != inbox.IngestOverloaded || last.Status != 503 {
		t.Errorf("observed %+v", last)
	}
}

// A screener that panics must not let the body through: the delivery is an internal
// failure and nothing is stored (it may be the redaction that failed). The panic is
// logged by type, since its value may hold the body.
func TestAPanicInTheScreenerStoresNothing(t *testing.T) {
	e, h, in, _ := screening(t, func(req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
		panic("cannot redact " + string(req.Body))
	})
	rec := e.post("/in/box", `4111111111111111`)
	if rec.Code != 500 || errorCode(t, rec) != "internal" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := e.count(in); n != 0 {
		t.Errorf("%d messages stored", n)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "panic in body screener") || strings.Contains(logs, "4111") {
		t.Errorf("log: %s", logs)
	}
	if ev := one(t, h.obsHooks); ev.Outcome != inbox.IngestError {
		t.Errorf("observed %+v", ev)
	}
	if used := e.server.ingestBudget.used; used != 0 {
		t.Errorf("%d bytes still held", used)
	}
	// The server goes on.
	h.mu.Lock()
	h.fn = nil
	h.mu.Unlock()
	if rec := e.post("/in/box", `ok`); rec.Code != 200 {
		t.Errorf("after the panic: %d", rec.Code)
	}
}

// The body is given to the screener without a copy. One that edits it in place and
// then says to keep it would store something the signature did not cover without
// saying so; that is refused.
func TestEditingTheBodyInPlaceWithoutReplacingIsRefused(t *testing.T) {
	e, _, in, _ := screening(t, func(req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
		copy(req.Body, "XXXX")
		return inbox.BodyDecision{}, nil
	})
	rec := e.post("/in/box", `4111`)
	if rec.Code != 500 || e.count(in) != 0 {
		t.Fatalf("%d %s, %d stored", rec.Code, rec.Body, e.count(in))
	}
	if !strings.Contains(e.logs.String(), "changed the body it was given") {
		t.Errorf("log: %s", e.logs.String())
	}
}

// An in-place edit that comes back as the replacement is a replacement like another.
func TestEditingTheBodyInPlaceAndReplacingIsAReplacement(t *testing.T) {
	e, _, in, _ := screening(t, func(req *inbox.BodyScreenRequest) (inbox.BodyDecision, error) {
		copy(req.Body, "XXXX")
		return inbox.BodyDecision{Replace: true, Body: req.Body}, nil
	})
	if rec := e.post("/in/box", `4111`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if m := e.messages(in)[0]; string(m.Body) != "XXXX" || !m.Modified {
		t.Errorf("%+v", m)
	}
}

// A duplicate that the Store recognises is reported with the message it repeats,
// modified or not, and the screener is asked about it, because only the Store can
// tell it is a repeat.
func TestADuplicateIsScreenedAndReportsTheEarlierMessage(t *testing.T) {
	e, h, _, _ := screening(t, replaceWith(`{"r":1}`))
	gh := e.newInbox("ghbox", "github", "s")
	body := `{"a":1}`
	for range 2 {
		e.post("/in/ghbox", body, "X-Hub-Signature-256", githubSig("s", body), "X-GitHub-Delivery", "d1")
	}
	if got := e.messages(gh); len(got) != 1 || !got[0].Modified {
		t.Fatalf("%+v", got)
	}
	if len(h.seenCalls()) != 2 {
		t.Errorf("screener calls: %d", len(h.seenCalls()))
	}
	evs := h.seen()
	if evs[1].Outcome != inbox.IngestDuplicate {
		t.Errorf("observed %+v", evs[1])
	}
}

// The MCP tools say a body was replaced, among what the server established, not among
// what the sender wrote.
func TestMCPSaysAMessageIsModified(t *testing.T) {
	m := projectMessage(wire.Message{Seq: 1, Modified: true})
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if string(top["modified"]) != "true" {
		t.Errorf("modified is not at the top of the MCP message: %s", b)
	}
	var untrusted map[string]json.RawMessage
	_ = json.Unmarshal(top["untrusted"], &untrusted)
	if _, ok := untrusted["modified"]; ok {
		t.Errorf("modified sits under untrusted: %s", b)
	}
}
