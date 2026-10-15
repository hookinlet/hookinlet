// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// The unverified deliveries an inbox keeps are capped, so that a leaked ingest key
// cannot fill it, and so cannot push genuine messages out under count retention.
func TestUnverifiedMessagesAreCapped(t *testing.T) {
	const maxUnverified = 3
	withCap := func(o *Options) { o.MaxUnverified = maxUnverified }
	forged := func(e *env, slug string) int {
		return e.post("/in/"+slug, `{"n":1}`, "X-Hub-Signature-256", githubSig("wrong", `{"n":1}`)).Code
	}
	storing := func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore }

	t.Run("a delivery over the cap is refused, not acknowledged, and not stored", func(t *testing.T) {
		e := newEnv(t, withCap)
		in := e.newInbox("capped", "github", "s", storing)
		for i := range maxUnverified {
			if code := forged(e, "capped"); code != http.StatusOK {
				t.Fatalf("unverified delivery %d under the cap: status %d", i+1, code)
			}
		}
		afterBefore := len(e.hooks.after())
		rec := e.post("/in/capped", `{"n":1}`, "X-Hub-Signature-256", githubSig("wrong", `{"n":1}`))
		if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "rate_limited" {
			t.Fatalf("status %d, %s", rec.Code, rec.Body.String())
		}
		if n := e.count(in); n != maxUnverified {
			t.Errorf("%d messages stored, want %d", n, maxUnverified)
		}
		if len(e.hooks.after()) != afterBefore {
			t.Error("a refused delivery was announced")
		}
	})

	t.Run("a full inbox still takes verified deliveries, and they keep their place", func(t *testing.T) {
		e := newEnv(t, withCap)
		in := e.newInbox("room", "github", "s", storing)
		for range maxUnverified + 2 {
			forged(e, "room")
		}
		body := `{"genuine":true}`
		if code := e.post("/in/room", body, "X-Hub-Signature-256", githubSig("s", body)).Code; code != http.StatusOK {
			t.Fatalf("a verified delivery into an inbox at its cap: status %d", code)
		}
		var verified, unverified int
		for _, m := range e.messages(in) {
			if m.Verified {
				verified++
			} else {
				unverified++
			}
		}
		if verified != 1 || unverified != maxUnverified {
			t.Errorf("%d verified and %d unverified kept, want 1 and %d", verified, unverified, maxUnverified)
		}
	})

	t.Run("the verifier none is capped too", func(t *testing.T) {
		e := newEnv(t, withCap)
		e.newInbox("open", "none", "")
		for range maxUnverified {
			if code := e.post("/in/open", `{}`).Code; code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
		}
		if code := e.post("/in/open", `{}`).Code; code != http.StatusTooManyRequests {
			t.Errorf("status %d over the cap, want 429", code)
		}
	})

	t.Run("one inbox at its cap does not close another", func(t *testing.T) {
		e := newEnv(t, withCap)
		e.newInbox("full", "none", "")
		e.newInbox("other", "none", "")
		for range maxUnverified + 1 {
			e.post("/in/full", `{}`)
		}
		if code := e.post("/in/other", `{}`).Code; code != http.StatusOK {
			t.Errorf("status %d", code)
		}
	})

	t.Run("retention makes room again", func(t *testing.T) {
		e := newEnv(t, withCap)
		in := e.newInbox("aging", "none", "", func(in *inbox.Inbox) { in.RetentionMaxCount = 1 })
		for range maxUnverified {
			e.post("/in/aging", `{}`)
		}
		if code := e.post("/in/aging", `{}`).Code; code != http.StatusTooManyRequests {
			t.Fatalf("status %d, want 429", code)
		}
		if _, err := e.store.Prune(t.Context(), serverNow); err != nil {
			t.Fatal(err)
		}
		if code := e.post("/in/aging", `{}`).Code; code != http.StatusOK {
			t.Errorf("status %d after retention freed room (%d stored)", code, e.count(in))
		}
	})

	t.Run("concurrent deliveries cannot overshoot the cap", func(t *testing.T) {
		e := newEnv(t, withCap)
		in := e.newInbox("race", "none", "")
		done := make(chan int, 20)
		for range 20 {
			go func() { done <- e.post("/in/race", `{}`).Code }()
		}
		ok := 0
		for range 20 {
			if <-done == http.StatusOK {
				ok++
			}
		}
		if ok != maxUnverified || e.count(in) != maxUnverified {
			t.Errorf("%d accepted and %d stored, want %d of each", ok, e.count(in), maxUnverified)
		}
	})

	t.Run("a negative MaxUnverified turns the cap off", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.MaxUnverified = -1 })
		e.newInbox("free", "none", "")
		for range DefaultMaxUnverified + 5 {
			if code := e.post("/in/free", `{}`).Code; code != http.StatusOK {
				t.Fatalf("status %d with the cap off", code)
			}
		}
	})

	t.Run("the default is on", func(t *testing.T) {
		e := newEnv(t)
		if e.server.opts.MaxUnverified != DefaultMaxUnverified || DefaultMaxUnverified <= 0 {
			t.Errorf("MaxUnverified defaults to %d", e.server.opts.MaxUnverified)
		}
	})

	t.Run("the refusal says nothing about the inbox's verifier", func(t *testing.T) {
		e := newEnv(t, withCap)
		e.newInbox("quiet", "github", "s", storing)
		for range maxUnverified {
			forged(e, "quiet")
		}
		rec := e.post("/in/quiet", `{"n":1}`, "X-Hub-Signature-256", githubSig("wrong", `{"n":1}`))
		for _, leak := range []string{"signature", "github", "X-Hub"} {
			if strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(leak)) {
				t.Errorf("the refusal mentions %q: %s", leak, rec.Body.String())
			}
		}
	})
}

func TestTheCapRefusalHasItsOwnMetricLabel(t *testing.T) {
	if got := deliveryResult(refuse(http.StatusTooManyRequests, codeUnverifiedLimit, "x")); got != "unverified_limit" {
		t.Errorf("label %q", got)
	}
	if got := deliveryResult(refuse(http.StatusTooManyRequests, "rate_limited", "x")); got != "refused_by_hook" {
		t.Errorf("label %q", got)
	}
}
