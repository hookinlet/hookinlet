// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// A request that promises a body and never sends it must not hold its connection for
// good, wherever it is sent. net/http reads what a handler leaves unread before it
// answers, and that read has no deadline unless the server gives it one: without it,
// anyone could hold any number of connections open, with no credential and no traffic,
// by sending them to a path that does not exist, to an unknown ingest key, or to the
// read API or MCP without a token.
func TestAStalledBodyCannotHoldAConnectionAnywhere(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.ReadBodyTimeout = 200 * time.Millisecond })
	addr := strings.TrimPrefix(base, "http://")
	promise := "Content-Length: 100\r\n\r\nx"
	for name, head := range map[string]string{
		"a health check":             "POST /healthz HTTP/1.1\r\nHost: x\r\n" + promise,
		"a path that does not exist": "POST /nowhere HTTP/1.1\r\nHost: x\r\n" + promise,
		"an unknown ingest key":      "POST /in/no-such-key HTTP/1.1\r\nHost: x\r\n" + promise,
		"MCP without a token":        "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" + promise,
		"MCP with a token":           "POST /mcp HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nAuthorization: Bearer " + ae.tokenA + "\r\n" + promise,
		"a read with a body":         "GET /v1/inboxes/agent-a HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + ae.tokenA + "\r\n" + promise,
		"a chunked body":             "POST /nowhere HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write([]byte(head)); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = io.Copy(io.Discard, conn) // until the server closes the connection
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("the connection was still held open after %v", time.Since(start).Round(time.Millisecond))
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("the connection was let go only after %v", d)
			}
		})
	}
}

// What runs long once it has its body, or with no body, is not cut short by the
// deadline for sending one.
func TestTheBodyDeadlineDoesNotEndLongOperations(t *testing.T) {
	ae, base, _ := streamEnv(t, func(o *Options) { o.ReadBodyTimeout = 200 * time.Millisecond })

	t.Run("an MCP wait", func(t *testing.T) {
		cs := connectMCP(t, base, ae.tokenA)
		start := time.Now()
		out := callTool(t, cs, "wait_for_messages", map[string]any{"inbox": "agent-a", "after": 0, "timeout_seconds": 1})
		if out.err != "" {
			t.Fatalf("the wait failed: %s", out.err)
		}
		if took := time.Since(start); took < 900*time.Millisecond {
			t.Errorf("a 1 second wait ended after %v", took)
		}
	})

	for name, extra := range map[string]string{
		"a stream":                       "\r\n",
		"a stream asked for with a body": "Content-Length: 2\r\n\r\n{}",
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", strings.TrimPrefix(base, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			req := "GET /v1/inboxes/agent-a/stream HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + ae.tokenA + "\r\n" + extra
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(conn)
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if line, err := r.ReadString('\n'); err != nil || !strings.Contains(line, " 200 ") {
				t.Fatalf("status line %q, %v", line, err)
			}
			time.Sleep(600 * time.Millisecond) // well past the deadline for a body
			ae.post("/in/agent-a", `{"late":"`+name+`"}`)
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					t.Fatalf("the stream ended before delivering a message sent after the body deadline: %v", err)
				}
				if strings.Contains(line, `"late":"`+name+`"`) {
					return
				}
			}
		})
	}
}
