// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func TestALogThrottleWritesOncePerIntervalAndCountsWhatItLeftOut(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newLogThrottle(time.Minute)
	th.now = func() time.Time { return now }

	if ok, skipped := th.allow(1); !ok || skipped != 0 {
		t.Fatalf("the first occurrence: write %v, skipped %d", ok, skipped)
	}
	for range 99 {
		now = now.Add(100 * time.Millisecond)
		if ok, _ := th.allow(1); ok {
			t.Fatal("an occurrence within the interval was written")
		}
	}
	if ok, skipped := th.allow(2); !ok || skipped != 0 { // another inbox has its own
		t.Errorf("another inbox: write %v, skipped %d", ok, skipped)
	}
	now = now.Add(time.Minute)
	if ok, skipped := th.allow(1); !ok || skipped != 99 {
		t.Errorf("after the interval: write %v, skipped %d; want a line saying 99 were left out", ok, skipped)
	}
	if ok, skipped := th.allow(1); ok || skipped != 0 {
		t.Errorf("right after that: write %v, skipped %d", ok, skipped)
	}
}

func TestALogThrottleDoesNotRememberEveryInboxForGood(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newLogThrottle(time.Minute)
	th.now = func() time.Time { return now }
	for id := range throttleKeep {
		th.allow(inbox.ID(id))
	}
	now = now.Add(2 * time.Minute)
	th.allow(inbox.ID(throttleKeep + 1)) // full, and all the others are idle
	if n := len(th.seen); n != 1 {
		t.Errorf("%d inboxes remembered after an idle interval, want 1", n)
	}
}

// The line an operator is told once carries how many followed it unlogged, and every
// refusal is still counted in the metrics.
func TestTheCapRefusalLineSaysHowManyWereLeftOut(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MaxUnverified = 2 })
	now := time.Unix(2_000_000, 0)
	e.server.capLog.now = func() time.Time { return now }
	e.newInbox("capped", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	send := func() *httptest.ResponseRecorder {
		return e.post("/in/capped", `{"n":1}`, "X-Hub-Signature-256", githubSig("wrong", `{"n":1}`))
	}
	for range 2 {
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("under the cap: %d", rec.Code)
		}
	}
	for range 50 {
		if rec := send(); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("at the cap: %d", rec.Code)
		}
	}
	lines := func() []map[string]any {
		var out []map[string]any
		for _, l := range strings.Split(e.logs.String(), "\n") {
			if strings.Contains(l, "too many unverified messages") {
				var m map[string]any
				if err := json.Unmarshal([]byte(l), &m); err != nil {
					t.Fatal(err)
				}
				out = append(out, m)
			}
		}
		return out
	}
	if got := lines(); len(got) != 1 || got[0]["refusals_not_logged"] != nil {
		t.Fatalf("after 50 refusals: %d lines %v, want the first alone", len(got), got)
	}
	now = now.Add(time.Minute)
	send()
	got := lines()
	if len(got) != 2 || got[1]["refusals_not_logged"] != float64(49) {
		t.Fatalf("a minute later: %v, want a second line saying 49 were left out", got)
	}
	var m strings.Builder
	_ = e.server.writeMetrics(&m)
	if !strings.Contains(m.String(), `hookinlet_deliveries_total{result="unverified_limit"} 51`) {
		t.Errorf("the metrics do not count all 51 refusals:\n%s", m.String())
	}
}

// brokenWriter is a response writer whose client has gone away.
type brokenWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (b brokenWriter) Write([]byte) (int, error) { return 0, b.err }

// A client that hangs up while its answer is written is not an error of the server's.
func TestAClientThatHungUpIsNotLoggedAsAnError(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		level string
	}{
		{"broken pipe", &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)}, "DEBUG"},
		{"connection reset", &net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.ECONNRESET)}, "DEBUG"},
		{"closed connection", net.ErrClosed, "DEBUG"},
		{"anything else", errors.New("the disk melted"), "ERROR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			rec := brokenWriter{httptest.NewRecorder(), c.err}
			func() {
				defer func() { _ = recover() }() // a body that broke part way aborts the handler
				e.server.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
			}()
			var level string
			for _, l := range strings.Split(e.logs.String(), "\n") {
				if strings.Contains(l, `"response failed"`) {
					var m map[string]any
					if err := json.Unmarshal([]byte(l), &m); err != nil {
						t.Fatal(err)
					}
					level, _ = m["level"].(string)
				}
			}
			if level != c.level {
				t.Errorf("logged at %q, want %q\n%s", level, c.level, e.logs.String())
			}
		})
	}
}
