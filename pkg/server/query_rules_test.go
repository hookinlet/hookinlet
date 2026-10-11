// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// The query strings the read API and the stream refuse. The document says which
// parameters exist and what each may hold; these are the inputs that have been
// worth writing down, each of which must be refused naming the parameter.

func TestMessageQueriesThatAreRefused(t *testing.T) {
	e := newAgentEnv(t)
	tests := []struct{ raw, param string }{
		{"limt=5", "limt"},
		{"token=abc", "token"},
		{"access_token=abc", "access_token"},
		{"After=1", "After"},
		{"after=1&after=2", "after"},
		{"limit=1&limit=2", "limit"},
		{"verified=true&verified=false", "verified"},
		{"fields=summary&fields=full", "fields"},
		{"after=-1", "after"},
		{"after=+1", "after"},
		{"after=1.5", "after"},
		{"after=1e3", "after"},
		{"after=0x10", "after"},
		{"after=%20", "after"},
		{"after=", "after"},
		{"after=abc", "after"},
		{"after=١٢٣", "after"},
		{"after=99999999999999999999", "after"},
		{"before=-5", "before"},
		{"before=", "before"},
		{"limit=0", "limit"},
		{"limit=-1", "limit"},
		{"limit=", "limit"},
		{"limit=ten", "limit"},
		{"type=", "type"},
		{"type=a&type=", "type"},
		{"type=" + strings.Repeat("a", 257), "type"},
		{"type=%ff", "type"},
		{"since=yesterday", "since"},
		{"since=2026-03-01", "since"},
		{"since=2026-03-01T12:00:00", "since"},
		{"since=1700000000", "since"},
		{"until=", "until"},
		{"until=2026-13-01T00:00:00Z", "until"},
		{"verified=yes", "verified"},
		{"verified=1", "verified"},
		{"verified=TRUE", "verified"},
		{"verified=", "verified"},
		{"delivery_id=", "delivery_id"},
		{"delivery_id=" + strings.Repeat("d", 257), "delivery_id"},
		{"fields=body", "fields"},
		{"fields=SUMMARY", "fields"},
		{"after=1&before=5", ""},
		{"since=2026-03-02T00:00:00Z&until=2026-03-01T00:00:00Z", ""},
		{"since=2026-03-01T00:00:00Z&until=2026-03-01T00:00:00Z", ""},
	}
	for _, tt := range tests {
		rec := e.get("/v1/inboxes/agent-a/messages?"+tt.raw, e.tokenA)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_parameter"`) {
			t.Errorf("%q: %d %s", tt.raw, rec.Code, rec.Body)
			continue
		}
		if tt.param != "" && !strings.Contains(rec.Body.String(), `parameter \"`+tt.param+`\"`) {
			t.Errorf("%q: the answer does not name %q: %s", tt.raw, tt.param, rec.Body)
		}
		if len(rec.Body.String()) > 400 {
			t.Errorf("%q: the answer repeats too much of the request: %d bytes", tt.raw, rec.Body.Len())
		}
	}
	// Exactly the maximum number of types is fine; one more is not.
	var parts []string
	for i := range inbox.MaxQueryTypes + 1 {
		parts = append(parts, "type=t"+itoa(i))
	}
	if rec := e.get("/v1/inboxes/agent-a/messages?"+strings.Join(parts, "&"), e.tokenA); rec.Code != http.StatusBadRequest {
		t.Errorf("an unbounded number of types: %d", rec.Code)
	}
	if rec := e.get("/v1/inboxes/agent-a/messages?"+strings.Join(parts[:inbox.MaxQueryTypes], "&"), e.tokenA); rec.Code != http.StatusOK {
		t.Errorf("exactly the maximum: %d %s", rec.Code, rec.Body)
	}
}

func TestStreamQueriesThatAreRefused(t *testing.T) {
	ae, base, _ := streamEnv(t)
	tests := []struct {
		raw  string
		last []string
		bad  string
	}{
		{"limit=5", nil, "limit"},
		{"since=2026-01-01T00:00:00Z", nil, "since"},
		{"fields=summary", nil, "fields"},
		{"before=5", nil, "before"},
		{"after=1&after=2", nil, "after"},
		{"after=-1", nil, "after"},
		{"after=abc", nil, "after"},
		{"after=", nil, "after"},
		{"type=", nil, "type"},
		{"verified=maybe", nil, "verified"},
		{"verified=1", nil, "verified"},
		{"", []string{"abc"}, "Last-Event-ID"},
		{"", []string{"-5"}, "Last-Event-ID"},
		{"", []string{"1.5"}, "Last-Event-ID"},
		{"", []string{"5", "6"}, "Last-Event-ID"},
		{"", []string{"99999999999999999999"}, "Last-Event-ID"},
	}
	for _, tt := range tests {
		hdr := http.Header{}
		for _, l := range tt.last {
			hdr.Add("Last-Event-ID", l)
		}
		status, body := streamStatus(t, ae.streamURL(base, "agent-a", tt.raw), ae.tokenA, hdr)
		if status != http.StatusBadRequest || !strings.Contains(body, tt.bad) {
			t.Errorf("%q %v: %d %s, want a 400 naming %q", tt.raw, tt.last, status, body, tt.bad)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// streamStatus opens a stream and reports how it was answered. A refused one is
// answered at once, so the body can be read in full.
func streamStatus(t *testing.T, url, tok string, hdr http.Header) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := sseHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, ""
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
