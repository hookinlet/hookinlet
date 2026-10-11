// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
)

// A client that has stopped reading leaves the server blocked in a write. That
// must not hold up shutdown, and above all must not push shutdown into the
// fallback that closes connections, which would cut off deliveries still being
// stored.
func TestAStalledStreamClientCannotBlockShutdown(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	e := newAgentEnv2(t, func(o *Options) { o.ShutdownTimeout = 2 * time.Second })
	e.hooks.before = func(*inbox.Inbox, int64) error {
		select {
		case entered <- struct{}{}:
			<-release // only the first delivery waits
		default:
		}
		return nil
	}
	tok := e.tokenFor(e.a)
	// A backlog far larger than any socket buffer, so the server must block.
	for range 400 {
		if _, _, err := e.store.Append(t.Context(), e.a.ID, &inbox.NewMessage{Method: "POST", Verified: true, Body: bytes.Repeat([]byte("p"), 60<<10)}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := newListener(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, shutdown := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.server.Serve(ctx, l) }()
	addr := l.Addr().String()

	// A client that reads the headers and then never reads again.
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(2048)
	}
	fmt.Fprintf(conn, "GET /v1/inboxes/agent-a/stream?after=0 HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", tok)
	buf := make([]byte, 512)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the server is now blocked writing to us

	// A delivery that is mid-way through being stored when shutdown begins.
	result := make(chan int, 1)
	go func() {
		status, _ := do2(t, http.MethodPost, "http://"+addr+"/in/agent-a", nil, "in flight")
		result <- status
	}()
	<-entered

	begin := time.Now()
	shutdown()
	time.Sleep(300 * time.Millisecond)
	select {
	case status := <-result:
		t.Fatalf("shutdown cut off the in-flight delivery (status %d)", status)
	default:
	}
	close(release)
	if status := <-result; status != http.StatusOK {
		t.Errorf("the in-flight delivery got %d", status)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v: a stalled client forced the fallback that closes connections", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("shutdown took %v with a stalled client (timeout 2s)", d)
	}
	if n := e.count(e.a); n != 401 {
		t.Errorf("%d messages stored, want 401 (400 backlog + the in-flight delivery)", n)
	}
}

// Rotating a token is the remedy for a leaked one. A stream opened with the old
// token must not go on receiving every future message.
func TestRotatingATokenEndsStreamsOpenedWithTheOldOne(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.StreamRevalidateInterval = 100 * time.Millisecond })
	old := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	admin := openStream(t, ae.streamURL(base, "agent-a", ""), testAdminToken, nil)
	other := openStream(t, ae.streamURL(base, "agent-b", ""), ae.tokenB, nil)
	old.next(time.Second)
	admin.next(time.Second)
	other.next(time.Second)

	newToken := ae.tokenFor(ae.a) // rotates: the old token no longer works
	if !old.ended(3 * time.Second) {
		t.Fatal("a stream opened with a rotated-out token is still open")
	}
	fresh := openStream(t, ae.streamURL(base, "agent-a", ""), newToken, nil)
	if fresh.status != 200 {
		t.Errorf("the new token: status %d", fresh.status)
	}
	if c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA+"", nil); c.status != 401 {
		t.Errorf("the old token reconnecting: status %d, want 401", c.status)
	}

	// Streams that are still authorized carry on.
	ae.post("/in/agent-a", "after rotation")
	if _, ok := admin.nextEvent(2 * time.Second); !ok {
		t.Error("the admin stream was ended by an unrelated rotation")
	}
	if _, ok := fresh.nextEvent(2 * time.Second); !ok {
		t.Error("the stream with the new token got nothing")
	}
	select {
	case <-other.done:
		t.Error("another inbox's stream was ended")
	default:
	}
}

// Deleting an inbox publishes nothing, so a stream that only waits for signals
// would idle forever on a dead inbox. It must notice on its own.
func TestADeletedInboxEndsItsStreamWithoutAnySignal(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.StreamRevalidateInterval = 100 * time.Millisecond })
	c := openStream(t, ae.streamURL(base, "agent-b", ""), ae.tokenB, nil)
	c.next(time.Second)
	if err := ae.store.DeleteInbox(t.Context(), ae.b.ID); err != nil {
		t.Fatal(err)
	}
	if !c.ended(3 * time.Second) {
		t.Error("the stream stayed open (and kept its slot) after its inbox was deleted")
	}
	// The slug is free again; a new inbox must not inherit the dead stream.
	ae.newInbox("agent-b", "trusted", "")
}

// flaky is a Store whose lookups fail on demand.
type flaky struct {
	inbox.Store
	failInboxes atomic.Bool
	failTokens  atomic.Bool
	failQuery   atomic.Bool
}

func (f *flaky) Query(ctx context.Context, id inbox.ID, q inbox.Query) (*inbox.Page, error) {
	if f.failQuery.Load() {
		return nil, errFlaky
	}
	return f.Store.Query(ctx, id, q)
}

var errFlaky = errors.New("the database is having a moment")

func (f *flaky) GetInbox(ctx context.Context, id inbox.ID) (*inbox.Inbox, error) {
	if f.failInboxes.Load() {
		return nil, errFlaky
	}
	return f.Store.GetInbox(ctx, id)
}

func (f *flaky) GetInboxByTokenHash(ctx context.Context, h []byte) (*inbox.Inbox, error) {
	if f.failTokens.Load() {
		return nil, errFlaky
	}
	return f.Store.GetInboxByTokenHash(ctx, h)
}

// Not being able to check is not the same as being told no. A passing database
// error must not disconnect every agent, whichever of the two checks hits it.
func TestATransientErrorWhileRecheckingDoesNotEndStreams(t *testing.T) {
	for _, failing := range []string{"token lookup", "inbox lookup", "both"} {
		t.Run(failing, func(t *testing.T) {
			fl := &flaky{}
			// The authenticator is built on the flaky store, so its lookup can fail too.
			e := newEnv(t,
				func(o *Options) { o.Notifier = local.New() },
				func(o *Options) { fl.Store = o.Store; o.Store = fl },
				withAuth,
				func(o *Options) { o.StreamRevalidateInterval = 50 * time.Millisecond },
			)
			in := e.newInbox("agent-a", "trusted", "")
			ae := &agentEnv{env: e, a: in}
			ae.tokenA = ae.tokenFor(in)
			base := serveOn(t, e)

			c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
			c.next(time.Second)
			fl.failTokens.Store(failing == "token lookup" || failing == "both")
			fl.failInboxes.Store(failing == "inbox lookup" || failing == "both")
			time.Sleep(400 * time.Millisecond) // several rechecks, all failing
			select {
			case <-c.done:
				t.Fatal("a stream was ended because the store could not answer a recheck")
			default:
			}
			fl.failTokens.Store(false)
			fl.failInboxes.Store(false)
			ae.post("/in/agent-a", "still here")
			if _, ok := c.nextEvent(3 * time.Second); !ok {
				t.Error("the stream stopped delivering after the store recovered")
			}
		})
	}
}

// An admin token does not belong to the inbox, so deleting the inbox does not
// invalidate it. The stream must notice the inbox is gone by looking.
func TestADeletedInboxEndsAnAdminStreamToo(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.StreamRevalidateInterval = 100 * time.Millisecond })
	c := openStream(t, ae.streamURL(base, "agent-b", ""), testAdminToken, nil)
	c.next(time.Second)
	if err := ae.store.DeleteInbox(t.Context(), ae.b.ID); err != nil {
		t.Fatal(err)
	}
	if !c.ended(3 * time.Second) {
		t.Error("an admin stream stayed open on an inbox that was deleted")
	}
}

// One token must not be able to use every stream slot.
func TestOneInboxCannotTakeEveryStream(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.MaxStreams = 5; o.MaxStreamsPerInbox = 2 })
	a1 := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	a2 := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	a3 := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	if a1.status != 200 || a2.status != 200 || a3.status != http.StatusServiceUnavailable {
		t.Fatalf("statuses %d, %d, %d; want 200, 200, 503", a1.status, a2.status, a3.status)
	}
	if b := openStream(t, ae.streamURL(base, "agent-b", ""), ae.tokenB, nil); b.status != 200 {
		t.Errorf("another inbox could not open a stream while the first held its share: status %d", b.status)
	}
	a1.close()
	<-a1.done
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil); c.status == 200 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("a slot was not freed when a stream closed")
}

func TestAClientAheadOfTheInboxIsToldSo(t *testing.T) {
	ae, base, _ := streamEnv(t)
	ae.post("/in/agent-a", "one")
	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, http.Header{"Last-Event-Id": {"999999"}})
	c.next(time.Second) // retry
	it, ok := c.next(2 * time.Second)
	if !ok || it.kind != "comment" || !strings.Contains(it.data, "ahead") || !strings.Contains(it.data, "999999") {
		t.Fatalf("first item after the retry hint = %+v; want a comment saying the position is ahead of the inbox", it)
	}
	// A client that is not ahead hears nothing of the kind.
	d := openStream(t, ae.streamURL(base, "agent-a", "after=1"), ae.tokenA, nil)
	d.next(time.Second)
	if it, ok := d.next(300 * time.Millisecond); ok && it.kind == "comment" && strings.Contains(it.data, "ahead") {
		t.Errorf("a client at the head was told it was ahead: %+v", it)
	}
}

type failingQuery struct {
	inbox.Store
	fail atomic.Bool
}

func (f *failingQuery) Query(ctx context.Context, id inbox.ID, q inbox.Query) (*inbox.Page, error) {
	if f.fail.Load() {
		return nil, errors.New("disk on fire")
	}
	return f.Store.Query(ctx, id, q)
}

func TestAStoreFailureMidStreamIsLoggedAsAnError(t *testing.T) {
	fq := &failingQuery{}
	n := newAgentEnv2(t, func(o *Options) { o.Notifier = local.New() }, func(o *Options) { fq.Store = o.Store; o.Store = fq })
	ae := &agentEnv{env: n.env, a: n.a}
	ae.tokenA = n.tokenFor(n.a)
	base := serveOn(t, ae.env)

	c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil)
	c.next(time.Second)
	fq.fail.Store(true)
	ae.post("/in/agent-a", "x")
	if !c.ended(3 * time.Second) {
		t.Fatal("the stream survived a store failure")
	}
	logs := ae.logs.String()
	if !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, "disk on fire") {
		t.Errorf("a store failure on a stream was not logged as an error: %s", logs)
	}
	// A client leaving is not an error.
	fq.fail.Store(false)
	quiet := newAgentEnv(t)
	qbase := serveOn(t, quiet.env)
	qc := openStream(t, quiet.streamURL(qbase, "agent-a", ""), quiet.tokenA, nil)
	qc.next(time.Second)
	qc.close()
	<-qc.done
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(quiet.logs.String(), `"level":"ERROR"`) {
		t.Errorf("a client disconnecting was logged as an error: %s", quiet.logs.String())
	}
}

// ctxNotifier records whether the context it is given can be cancelled.
type ctxNotifier struct {
	inbox.NopNotifier
	cancellable atomic.Int32
	calls       atomic.Int32
}

func (n *ctxNotifier) Publish(ctx context.Context, _ inbox.ID, _ int64) {
	n.calls.Add(1)
	if ctx.Done() != nil {
		n.cancellable.Add(1)
	}
}

// The request that stored a message is over, or about to be, once the notifier is
// told. A distributed notifier that honored a cancelled context would drop the
// signal, so the notifier is handed a context that cannot be cancelled.
func TestPublishIsNotGivenTheRequestsContext(t *testing.T) {
	n := &ctxNotifier{}
	e := newEnv(t, func(o *Options) { o.Notifier = n })
	e.newInbox("announce-ctx", "trusted", "")
	e.post("/in/announce-ctx", "x")
	if n.calls.Load() != 1 {
		t.Fatalf("Publish was called %d times", n.calls.Load())
	}
	if n.cancellable.Load() != 0 {
		t.Error("Publish received the ingest request's cancellable context")
	}
}

// Replaying a backlog of large messages must still deliver every one, in
// order, exactly once, however the small stream pages cut it up.
func TestReplayOfLargeMessagesIsExact(t *testing.T) {
	ae, base, _ := streamEnv(t)
	const count = 60
	for i := range count {
		body := bytes.Repeat([]byte{byte('a' + i%26)}, 100<<10)
		if _, _, err := ae.store.Append(t.Context(), ae.a.ID, &inbox.NewMessage{Method: "POST", Verified: true, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	c := openStream(t, ae.streamURL(base, "agent-a", "after=0"), ae.tokenA, nil)
	// Read one more than expected, so a stream that repeats itself is reported as
	// such instead of being waited on forever.
	var events []sseItem
	for len(events) <= count {
		it, ok := c.nextEvent(2 * time.Second)
		if !ok {
			break
		}
		events = append(events, it)
	}
	if len(events) != count {
		ids := make([]string, 0, len(events))
		for _, it := range events {
			ids = append(ids, it.id)
		}
		t.Fatalf("%d events for %d large messages (repeated or missing); ids: %v", len(events), count, ids)
	}
	for i, it := range events {
		if idOf(t, it) != int64(i+1) {
			t.Fatalf("event %d has id %s", i, it.id)
		}
	}
}

// A client that disappears while the server is writing to it is the client's
// doing. It must not be logged as a failure on the server's side.
func TestAClientVanishingMidWriteIsNotAServerError(t *testing.T) {
	ae, base, _ := streamEnv(t)
	tok := ae.tokenA
	for range 200 {
		if _, _, err := ae.store.Append(t.Context(), ae.a.ID, &inbox.NewMessage{Method: "POST", Verified: true, Body: bytes.Repeat([]byte("v"), 60<<10)}); err != nil {
			t.Fatal(err)
		}
	}
	addr := strings.TrimPrefix(base, "http://")
	for range 4 {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET /v1/inboxes/agent-a/stream?after=0 HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", tok)
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Read(buf); err != nil {
			t.Fatal(err)
		}
		// Let the server fill the socket buffers and block inside a write, then reset
		// the connection. Resetting at once would cancel the request before the
		// server ever wrote, and never reach the write-failure path.
		time.Sleep(300 * time.Millisecond)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
	time.Sleep(500 * time.Millisecond)
	if logs := ae.logs.String(); strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("clients disconnecting mid-write were logged as errors: %s", logs)
	}
	// And the slots they held were released.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := openStream(t, ae.streamURL(base, "agent-a", ""), ae.tokenA, nil); c.status == 200 {
			return
		}
	}
	t.Error("stream slots were not released after the clients vanished")
}
