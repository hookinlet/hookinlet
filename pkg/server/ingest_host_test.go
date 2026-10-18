// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func TestRequestHost(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"hooks.example.com", "hooks.example.com"},
		{"Hooks.Example.COM", "hooks.example.com"},
		{"hooks.example.com.", "hooks.example.com"},
		{"HOOKS.example.com.", "hooks.example.com"},
		{"hooks.example.com:8443", "hooks.example.com:8443"},
		{"Hooks.Example.com.:8443", "hooks.example.com:8443"},
		{"hooks.example.com:", "hooks.example.com"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"[::1]", "[::1]"},
		{"[::1]:8080", "[::1]:8080"},
		{"[FE80::1]:80", "[fe80::1]:80"},
		{"localhost", "localhost"},
		{"", ""},
	} {
		r := &http.Request{Host: c.in, Header: http.Header{}}
		if got := requestHost(r); got != c.want {
			t.Errorf("Host %q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRequestHostIgnoresForwardingHeaders(t *testing.T) {
	r := &http.Request{Host: "Real.example.com", Header: http.Header{}}
	r.Header.Set("X-Forwarded-Host", "spoofed.example.com")
	r.Header.Set("Forwarded", "host=spoofed.example.com")
	r.Header.Set("X-Original-Host", "spoofed.example.com")
	r.Header.Set("X-Host", "spoofed.example.com")
	if got := requestHost(r); got != "real.example.com" {
		t.Errorf("got %q", got)
	}
}

func TestBeforeIngestSeesTheHost(t *testing.T) {
	e, obs := observed(t)
	e.newInbox("multi", "trusted", "")

	for _, c := range []struct {
		name, host, want string
		headers          http.Header
	}{
		{"as sent", "hooks.example.com", "hooks.example.com", nil},
		{"normalised", "Hooks.Example.COM.:8443", "hooks.example.com:8443", nil},
		{"not forwarded", "real.example.com", "real.example.com", http.Header{
			"X-Forwarded-Host": {"spoofed.example.com"}, "Forwarded": {"host=spoofed.example.com"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(e.hooks.requests)
			beforeEvents := len(obs.seen())
			rec := e.do(req{path: "/in/multi/p", body: "x", host: c.host, headers: c.headers})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			e.hooks.mu.Lock()
			got := e.hooks.requests[before:]
			e.hooks.mu.Unlock()
			if len(got) != 1 || got[0].Host != c.want {
				t.Errorf("BeforeIngest saw %+v, want host %q", got, c.want)
			}
			evs := obs.seen()[beforeEvents:]
			if len(evs) != 1 || evs[0].Host != c.want {
				t.Errorf("ObserveIngest saw %+v, want host %q", evs, c.want)
			}
		})
	}
}

// A hook's ErrNotFound and an unknown key must not differ in anything a sender can see.
func TestHookNotFoundIsAnsweredLikeAnUnknownKey(t *testing.T) {
	for _, hookErr := range []error{
		inbox.ErrNotFound,
		fmt.Errorf("wrong host for this inbox: %w", inbox.ErrNotFound),
		inbox.RefuseAfter(fmt.Errorf("not on this host: %w", inbox.ErrNotFound), 90*time.Second),
	} {
		e := newEnv(t)
		e.newInbox("elsewhere", "trusted", "")
		e.hooks.mu.Lock()
		e.hooks.before = func(*inbox.Inbox, int64) error { return hookErr }
		e.hooks.mu.Unlock()

		for _, c := range []struct{ method, known, unknown string }{
			{"POST", "/in/elsewhere", "/in/nothing-here"},
			{"PUT", "/in/elsewhere", "/in/nothing-here"},
			{"POST", "/in/elsewhere/a/b", "/in/nothing-here/a/b"},
			{"PUT", "/in/elsewhere/a/b", "/in/nothing-here/a/b"},
		} {
			name := fmt.Sprintf("%v %s %s", hookErr, c.method, c.known)
			// The unknown key is asked first and the hook never runs for it.
			logs0 := len(e.logs.String())
			unk := e.do(req{method: c.method, path: c.unknown, body: "payload"})
			logs1 := e.logs.String()
			hook := e.do(req{method: c.method, path: c.known, body: "payload"})
			logs2 := e.logs.String()

			if unk.Code != http.StatusNotFound || hook.Code != http.StatusNotFound {
				t.Fatalf("%s: statuses %d and %d, want 404", name, unk.Code, hook.Code)
			}
			if !bytes.Equal(unk.Body.Bytes(), hook.Body.Bytes()) {
				t.Errorf("%s: bodies differ: %q vs %q", name, unk.Body, hook.Body)
			}
			if a, b := visibleHeaders(unk), visibleHeaders(hook); !slices.Equal(a, b) {
				t.Errorf("%s: headers differ:\n%q\n%q", name, a, b)
			}
			if got := hook.Header().Get("Retry-After"); got != "" {
				t.Errorf("%s: Retry-After %q on a 404", name, got)
			}
			// No log line above debug beyond what the unknown key gets.
			if a, b := levelsAbove(t, logs1[logs0:]), levelsAbove(t, logs2[len(logs1):]); !slices.Equal(a, b) {
				t.Errorf("%s: log lines differ above debug:\n%q\n%q", name, a, b)
			}
		}
		if e.count(e.mustInbox("elsewhere")) != 0 {
			t.Error("a delivery the hook answered 404 was stored")
		}
	}
}

func (e *env) mustInbox(slug string) *inbox.Inbox {
	e.t.Helper()
	in, err := e.store.ResolveIngestKey(e.t.Context(), slug)
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

// visibleHeaders lists a response's headers other than the ones that differ per
// request (Date, X-Request-Id), with the content length, as sorted "name: value".
func visibleHeaders(rec *httptest.ResponseRecorder) []string {
	var out []string
	for k, vs := range rec.Header() {
		if k == "Date" || k == "X-Request-Id" {
			continue
		}
		for _, v := range vs {
			out = append(out, k+": "+v)
		}
	}
	out = append(out, fmt.Sprintf("length: %d", rec.Body.Len()))
	slices.Sort(out)
	return out
}

// levelsAbove lists "level message" of the JSON log lines in s above debug.
func levelsAbove(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var rec struct{ Level, Msg string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec.Level == "DEBUG" {
			continue
		}
		out = append(out, rec.Level+" "+rec.Msg)
	}
	return out
}

func TestHookRefusalsOtherThanNotFoundAreUnchanged(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{inbox.ErrRateLimited, 429, "rate_limited"},
		{inbox.ErrQuotaExceeded, 402, "quota_exceeded"},
		{inbox.ErrForbidden, 403, "forbidden"},
		{fmt.Errorf("something broke"), 500, "internal"},
	} {
		e := newEnv(t)
		e.newInbox("gated", "trusted", "")
		e.hooks.mu.Lock()
		e.hooks.before = func(*inbox.Inbox, int64) error { return inbox.RefuseAfter(c.err, 30*time.Second) }
		e.hooks.mu.Unlock()
		rec := e.do(req{path: "/in/gated", body: "x"})
		if rec.Code != c.status || errorCode(t, rec) != c.code {
			t.Errorf("%v: status %d, %s", c.err, rec.Code, rec.Body)
		}
		wantRetry := ""
		if c.status != 500 {
			wantRetry = "30"
		}
		if got := rec.Header().Get("Retry-After"); got != wantRetry {
			t.Errorf("%v: Retry-After %q, want %q", c.err, got, wantRetry)
		}
	}
}

func TestIngestWithoutAHookIsUnchanged(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Hooks = nil })
	e.newInbox("plain", "trusted", "")
	if rec := e.do(req{path: "/in/plain", body: "x", host: "Any.Host.:81"}); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec := e.do(req{path: "/in/missing", body: "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}
