// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// A client that sends request after request on one connection and never reads the
// answers must not hold the connection for good: net/http writes the answers one after
// another, and a write that the client does not take blocks until a deadline ends it.
// Nothing here needs a credential except the MCP case, whose answers are held in memory.
func TestAClientThatDoesNotReadCannotHoldAConnection(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.ResponseTimeout = 300 * time.Millisecond })
	addr := strings.TrimPrefix(base, "http://")
	mcpCall := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for name, req := range map[string]string{ //nolint:gosec // requests, one with a test token
		"health checks":              "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n",
		"a path that does not exist": "GET /nowhere HTTP/1.1\r\nHost: x\r\n\r\n",
		"reads without a token":      "GET /v1/inboxes/agent-a HTTP/1.1\r\nHost: x\r\n\r\n",
		"an unknown ingest key":      "POST /in/no-such-key HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n",
		"MCP with a token": "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\n" +
			"Authorization: Bearer " + ae.tokenA + "\r\nContent-Length: " + strconv.Itoa(len(mcpCall)) + "\r\n\r\n" + mcpCall,
	} {
		t.Run(name, func(t *testing.T) {
			dialer := net.Dialer{Control: func(_, _ string, c rawConn) error { return smallReceiveBuffer(c) }}
			conn, err := dialer.DialContext(t.Context(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			// Send requests until the server stops reading them, which it does once it is
			// stuck writing answers the client does not take (or, on a busy machine, is
			// merely behind: then it gets stuck a little later).
			batch := []byte(strings.Repeat(req, 100))
			sent := 0
			for stuck := time.Now().Add(30 * time.Second); ; {
				if time.Now().After(stuck) {
					t.Fatal("the server never stopped reading requests")
				}
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				n, err := conn.Write(batch)
				sent += n
				if err != nil {
					break
				}
			}
			whole := sent / len(req)
			time.Sleep(time.Second) // longer than ResponseTimeout

			// Read what is there. A server that gave up on the client has closed the
			// connection, having answered only some of the requests. One that waited
			// answers every whole request it was sent, then waits for more.
			_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
			got, err := io.ReadAll(conn)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("the server was still waiting for the client after 20 seconds")
			}
			if answered := strings.Count(string(got), "HTTP/1.1 "); answered >= whole {
				t.Fatalf("the server answered all %d requests: it waited for the client to read", answered)
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
