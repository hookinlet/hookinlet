// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// hangupWatch is a listener that notices when the server closes a connection it
// accepted, so a test can tell that the server gave up on a client without reading
// anything from it, which would give the server room to carry on.
type hangupWatch struct {
	net.Listener
	mu     sync.Mutex
	closed map[string]chan struct{} // by the client's address
}

func (l *hangupWatch) hangup(clientAddr string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed == nil {
		l.closed = map[string]chan struct{}{}
	}
	if l.closed[clientAddr] == nil {
		l.closed[clientAddr] = make(chan struct{})
	}
	return l.closed[clientAddr]
}

func (l *hangupWatch) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &watchedConn{Conn: c, closed: l.hangup(c.RemoteAddr().String())}, nil
}

type watchedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *watchedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// A client that sends request after request on one connection and never reads the
// answers must not hold the connection for good: net/http writes the answers one after
// another, and a write that the client does not take blocks until a deadline ends it.
// Nothing here needs a credential except the MCP case, whose answers are held in memory.
//
// The test sends and does not read, and waits for the server to close the connection.
// How soon depends on how fast the machine is and how much the kernel will buffer, which
// is why it does not decide after a fixed time: it fails only if the server has not hung
// up well after it stopped taking requests, which a server waiting on the client never does.
func TestAClientThatDoesNotReadCannotHoldAConnection(t *testing.T) {
	// The server's send buffer is made small as well as the client's receive buffer. Without that a
	// Linux server parks megabytes of answers in the kernel and is never stuck writing: the
	// requests that cost it some work (an ingest, an MCP call) then only keep it busy.
	lc := net.ListenConfig{Control: func(_, _ string, c rawConn) error { return smallSendBuffer(c) }}
	ae, _ := streamEnvUnserved(t, func(o *Options) {
		o.ResponseTimeout = 300 * time.Millisecond
		o.ReadWaitTimeout = 100 * time.Millisecond
	})
	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	watch := &hangupWatch{Listener: inner}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = ae.server.Serve(ctx, watch) }()
	t.Cleanup(func() { cancel(); <-done })
	addr := inner.Addr().String()

	mcpCall := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for name, req := range map[string]string{ //nolint:gosec // requests, one with a test token
		"health checks":              "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n",
		"a path that does not exist": "GET /nowhere HTTP/1.1\r\nHost: x\r\n\r\n",
		"reads without a token":      "GET /v1/inboxes/agent-a HTTP/1.1\r\nHost: x\r\n\r\n",
		"an unknown ingest key":      "POST /in/no-such-key HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n",
		// With a credential, the early refusals are bound too: they are answered before any
		// operation has begun to build an answer.
		"a stream refused for its epoch, with a token": "GET /v1/inboxes/agent-a/stream?epoch=00000000000000000000000000000000 HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + ae.tokenA + "\r\n\r\n",
		"a bad parameter, with a token":                "GET /v1/inboxes/agent-a/messages?limit=abc HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + ae.tokenA + "\r\n\r\n",
		"MCP with a token": "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\n" +
			"Authorization: Bearer " + ae.tokenA + "\r\nContent-Length: " + strconv.Itoa(len(mcpCall)) + "\r\n\r\n" + mcpCall,
	} {
		t.Run(name, func(t *testing.T) {
			// The request is a real one: a connection that reads gets an answer.
			probe, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			_ = probe.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := probe.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			if resp, err := http.ReadResponse(bufio.NewReader(probe), nil); err != nil {
				t.Fatalf("no answer to the request: %v", err)
			} else {
				_ = resp.Body.Close()
			}
			_ = probe.Close()

			dialer := net.Dialer{Control: func(_, _ string, c rawConn) error { return smallReceiveBuffer(c) }}
			conn, err := dialer.DialContext(t.Context(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			hungUp := watch.hangup(conn.LocalAddr().String())

			// Send requests and read nothing. The server stops taking them once it is stuck
			// writing answers the client does not take (or, on a slow machine, is merely
			// behind: then it gets stuck a little later); a stuck server then gives up on the
			// client within the time the answer was allowed.
			const patience = 5 * time.Second // from the server not taking requests to its hanging up
			batch := []byte(strings.Repeat(req, 100))
			giveUp := time.Now().Add(60 * time.Second)
			var stalled time.Time // when the server stopped taking requests
		send:
			for {
				select {
				case <-hungUp:
					break send
				default:
				}
				if time.Now().After(giveUp) {
					t.Fatal("the server never hung up on a client that does not read")
				}
				if !stalled.IsZero() && time.Since(stalled) > patience {
					t.Fatalf("the server stopped taking requests %v ago and has not hung up on the client", time.Since(stalled).Round(time.Second))
				}
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_, err := conn.Write(batch)
				var ne net.Error
				switch {
				case err == nil:
					stalled = time.Time{}
				case errors.As(err, &ne) && ne.Timeout():
					if stalled.IsZero() {
						stalled = time.Now()
					}
				default: // reset or broken: the server has closed it
					select {
					case <-hungUp:
					case <-time.After(patience):
						t.Fatalf("the connection failed (%v) but the server did not close it", err)
					}
					break send
				}
			}
		})
	}
}

// The time an ingest takes to receive and store a delivery is not the client's: its
// answer gets ResponseTimeout from when it is ready, not from when the request came.
func TestASlowDeliveryStillGetsItsAnswer(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.ResponseTimeout = 300 * time.Millisecond })
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("POST /in/agent-a HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // the body takes longer to arrive than ResponseTimeout
	if _, err := conn.Write([]byte("}")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a delivery that was stored: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || ae.count(ae.a) != 1 {
		t.Errorf("status %d, %d stored", resp.StatusCode, ae.count(ae.a))
	}
}

// slowListStore takes a second to list inboxes.
type slowListStore struct{ inbox.Store }

func (s slowListStore) ListInboxes(ctx context.Context, p inbox.ListParams) ([]*inbox.Inbox, error) {
	time.Sleep(time.Second)
	return s.Store.ListInboxes(ctx, p)
}

// An operation that needs a credential may run long before it answers (an admin call
// over a large database), and the deadline every request starts with must not cut its
// answer off.
func TestAnAdminCallThatRunsLongStillGetsItsAnswer(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Store = slowListStore{o.Store}; o.ResponseTimeout = 300 * time.Millisecond }, withAuth)
	base := serveOn(t, e)
	status, body := do2(t, "GET", base+"/v1/admin/inboxes", http.Header{"Authorization": {"Bearer " + testAdminToken}}, "")
	if status != http.StatusOK {
		t.Errorf("status %d: %s", status, body)
	}
}
