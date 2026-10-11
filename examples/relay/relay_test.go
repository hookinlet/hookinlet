// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/server"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// hookinlet is a real server with one inbox, for the relay to read.
type hookinlet struct {
	url   string
	token string
	store *sqlite.Store
	in    *inbox.Inbox
}

const relaySecret = "relay-test-secret" //nolint:gosec // a value made up for a test

func newHookinlet(t *testing.T) *hookinlet {
	t.Helper()
	store, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{
		Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: store, SecretBox: box,
		Notifier: local.New(), Authenticator: token.NewAuthenticator(store, strings.Repeat("a", 40)),
		RetentionInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	tok, hash, err := token.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// A relay forwards only what a verifier checked, so the inbox checks a shared token.
	sealed, err := box.Seal([]byte(relaySecret))
	if err != nil {
		t.Fatal(err)
	}
	in := &inbox.Inbox{Slug: "relayed", Name: "relayed", Verifier: "token", VerifierSecret: sealed, AgentTokenHash: hash}
	if err := store.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	return &hookinlet{url: ts.URL, token: tok, store: store, in: in}
}

func (h *hookinlet) send(t *testing.T, body, contentType string, headers ...string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url+"/in/relayed", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Inbox-Token", relaySecret)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("send: %d", resp.StatusCode)
	}
}

// destination records what it is sent and answers with whatever status the test
// has chosen.
type destination struct {
	*httptest.Server
	mu     sync.Mutex
	got    []received
	status func(n int, r *http.Request) int // n counts requests from 1
	count  int
}

type received struct {
	body   string
	header http.Header
}

func newDestination(t *testing.T, status func(n int, r *http.Request) int) *destination {
	t.Helper()
	d := &destination{status: status}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.count++
		n := d.count
		code := 200
		if d.status != nil {
			code = d.status(n, r)
		}
		if code < 300 {
			d.got = append(d.got, received{string(b), r.Header.Clone()})
		}
		d.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *destination) bodies() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, r := range d.got {
		out = append(out, r.body)
	}
	return out
}

func (d *destination) requests() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

func relayFor(t *testing.T, h *hookinlet, d *destination, state string) *Relay {
	t.Helper()
	return &Relay{
		Source: h.url, Token: h.token, Inbox: "relayed", Destination: d.URL, StateFile: state, Start: "beginning",
		PollInterval: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// runUntil runs the relay until cond holds, then stops it and waits.
func runUntil(t *testing.T, r *Relay, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ok := cond()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ok {
		t.Fatal("the relay did not get there")
	}
}

func TestMessagesArriveInOrderWithTheirExactBytesAndHeaders(t *testing.T) {
	h := newHookinlet(t)
	exact := "{\"b\": 1,   \"a\":\"<&>\"}\n" // spacing and escapes that re-encoding would change
	h.send(t, exact, "application/json", "X-Hub-Signature-256", "sha256=abc", "X-Custom", "yes")
	h.send(t, "plain text", "text/plain")
	h.send(t, "\xff\xfe binary", "application/octet-stream")
	d := newDestination(t, nil)

	runUntil(t, relayFor(t, h, d, filepath.Join(t.TempDir(), "state")), func() bool { return len(d.bodies()) == 3 })

	if got := d.bodies(); got[0] != exact || got[1] != "plain text" || got[2] != "\xff\xfe binary" {
		t.Errorf("bodies = %q", got)
	}
	first := d.got[0].header
	if first.Get("X-Hub-Signature-256") != "sha256=abc" || first.Get("X-Custom") != "yes" {
		t.Errorf("the sender's headers were not passed on: %v", first)
	}
	if first.Get("Content-Type") != "application/json" || first.Get("X-Hookinlet-Seq") != "1" ||
		first.Get("X-Hookinlet-Inbox") != "relayed" || first.Get("X-Hookinlet-Verified") != "true" || first.Get("X-Hookinlet-Received-At") == "" {
		t.Errorf("hookinlet headers: %v", first)
	}
	if d.got[1].header.Get("X-Hookinlet-Seq") != "2" || d.got[2].header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("later headers: %v %v", d.got[1].header, d.got[2].header)
	}
	for _, name := range []string{"Authorization", "Connection"} {
		if first.Get(name) != "" {
			t.Errorf("%s was passed on: %q (it carries the agent token, or describes another connection)", name, first.Get(name))
		}
	}
}

func TestADestinationThatIsDownIsRetriedAndNothingIsSkipped(t *testing.T) {
	h := newHookinlet(t)
	for _, b := range []string{"one", "two", "three"} {
		h.send(t, b, "text/plain")
	}
	// Refuse the first four requests in various ways, then accept everything.
	refusals := []int{500, 503, 429, 408}
	d := newDestination(t, func(n int, _ *http.Request) int {
		if n <= len(refusals) {
			return refusals[n-1]
		}
		return 200
	})
	runUntil(t, relayFor(t, h, d, filepath.Join(t.TempDir(), "state")), func() bool { return len(d.bodies()) == 3 })
	if got := strings.Join(d.bodies(), ","); got != "one,two,three" {
		t.Errorf("bodies = %s: a message was skipped, repeated, or reordered", got)
	}
	if d.requests() < 7 {
		t.Errorf("only %d requests: the refused ones were not retried", d.requests())
	}
}

// One message the destination will never take must not hold up the rest.
func TestAMessageTheDestinationRefusesForGoodIsSkipped(t *testing.T) {
	h := newHookinlet(t)
	for _, b := range []string{"fine-1", "poison", "fine-2"} {
		h.send(t, b, "text/plain")
	}
	d := newDestination(t, func(_ int, r *http.Request) int {
		if r.Header.Get("X-Hookinlet-Seq") == "2" {
			return 400
		}
		return 200
	})
	r := relayFor(t, h, d, filepath.Join(t.TempDir(), "state"))
	runUntil(t, r, func() bool { return len(d.bodies()) == 2 })
	if got := strings.Join(d.bodies(), ","); got != "fine-1,fine-2" {
		t.Errorf("bodies = %s", got)
	}
	if d.requests() != 3 {
		t.Errorf("%d requests: the refused message was retried, or something else was", d.requests())
	}
}

// A sender chooses its headers, and a proxy in front of hookinlet adds some. Those
// that a destination might trust are not replayed.
func TestHeadersADestinationMightTrustAreNotPassedOn(t *testing.T) {
	h := newHookinlet(t)
	h.send(t, "x", "text/plain",
		"X-Hookinlet-Event-Type", "invoice.paid", "X-Hookinlet-Delivery-Id", "evt_forged", "X-Hookinlet-Verified", "false",
		"X-Forwarded-For", "10.0.0.1", "X-Forwarded-Proto", "https", "Forwarded", "for=10.0.0.1", "X-Real-Ip", "10.0.0.1", "Via", "1.1 proxy",
		"X-Signature", "keep-me", "X-Internal-Marker", "strip-me")
	d := newDestination(t, nil)
	r := relayFor(t, h, d, filepath.Join(t.TempDir(), "state"))
	r.StripHeaders = []string{"x-internal-marker"}
	runUntil(t, r, func() bool { return len(d.bodies()) == 1 })
	got := d.got[0].header
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip", "Via", "X-Internal-Marker", "X-Hookinlet-Event-Type", "X-Hookinlet-Delivery-Id"} {
		if v := got.Get(name); v != "" {
			t.Errorf("%s = %q was passed on", name, v)
		}
	}
	if got.Get("X-Signature") != "keep-me" {
		t.Errorf("the sender's signature header was lost: %v", got)
	}
	if got.Get("X-Hookinlet-Verified") != "true" || got.Get("X-Hookinlet-Seq") != "1" {
		t.Errorf("the relay's own headers are wrong (a sender's X-Hookinlet-Verified must not win): %v", got)
	}
}

// A mistyped address or an expired credential must not discard the backlog.
func TestAMisconfiguredDestinationLosesNothing(t *testing.T) {
	for _, status := range []int{301, 307, 401, 403, 404, 405} {
		h := newHookinlet(t)
		h.send(t, "one", "text/plain")
		h.send(t, "two", "text/plain")
		d := newDestination(t, func(n int, _ *http.Request) int {
			if n >= 4 { // the operator has corrected the address by now
				return 200
			}
			return status
		})
		runUntil(t, relayFor(t, h, d, filepath.Join(t.TempDir(), "state")), func() bool { return len(d.bodies()) == 2 })
		if got := strings.Join(d.bodies(), ","); got != "one,two" {
			t.Errorf("status %d: bodies = %s, want both messages to arrive once the destination is right", status, got)
		}
	}
}

func TestARestartCarriesOnWithoutRepeatingOrLosingAnything(t *testing.T) {
	h := newHookinlet(t)
	state := filepath.Join(t.TempDir(), "state")
	h.send(t, "one", "text/plain")
	h.send(t, "two", "text/plain")
	d := newDestination(t, nil)
	runUntil(t, relayFor(t, h, d, state), func() bool { return len(d.bodies()) == 2 })

	// The position is saved just after the delivery that moved it, so the file may
	// be a moment behind the last body the destination saw.
	var saved string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(state) //nolint:gosec // a temp path
		if saved = strings.TrimSpace(string(b)); saved == "2" {
			break
		}
	}
	if saved != "2" {
		t.Fatalf("state file = %q, want 2", saved)
	}
	h.send(t, "three", "text/plain")
	// A new relay, as after a restart: it starts "beginning" but the state wins.
	runUntil(t, relayFor(t, h, d, state), func() bool { return len(d.bodies()) == 3 })
	if got := strings.Join(d.bodies(), ","); got != "one,two,three" {
		t.Errorf("bodies = %s", got)
	}
}

func TestStartingFromNowSkipsWhatIsAlreadyThere(t *testing.T) {
	h := newHookinlet(t)
	h.send(t, "old-1", "text/plain")
	h.send(t, "old-2", "text/plain")
	d := newDestination(t, nil)
	r := relayFor(t, h, d, filepath.Join(t.TempDir(), "state"))
	r.Start = "now"

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	h.send(t, "new", "text/plain")
	deadline := time.Now().Add(5 * time.Second)
	for len(d.bodies()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := strings.Join(d.bodies(), ","); got != "new" {
		t.Errorf("bodies = %s, want only the message that arrived after the start", got)
	}
}

func TestUnverifiedMessagesAreNotForwardedUnlessAsked(t *testing.T) {
	h := newHookinlet(t)
	in, _ := h.store.GetInbox(t.Context(), h.in.ID)
	in.Verifier = "token"
	in.OnVerifyFail = inbox.OnVerifyFailStore
	in.VerifierSecret = nil
	// Seed one verified and one unverified message straight into the store.
	if err := h.store.UpdateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		body     string
		verified bool
	}{{"trusted", true}, {"forged", false}, {"trusted-2", true}} {
		if _, _, err := h.store.Append(t.Context(), h.in.ID, &inbox.NewMessage{Method: "POST", Body: []byte(m.body), Verified: m.verified, ContentType: "text/plain"}); err != nil {
			t.Fatal(err)
		}
	}
	d := newDestination(t, nil)
	runUntil(t, relayFor(t, h, d, filepath.Join(t.TempDir(), "state")), func() bool { return len(d.bodies()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if got := strings.Join(d.bodies(), ","); got != "trusted,trusted-2" {
		t.Errorf("bodies = %s: an unverified message was forwarded", got)
	}

	all := newDestination(t, nil)
	r := relayFor(t, h, all, filepath.Join(t.TempDir(), "state2"))
	r.IncludeUnverified = true
	runUntil(t, r, func() bool { return len(all.bodies()) == 3 })
	if got := all.got[1].header.Get("X-Hookinlet-Verified"); got != "false" {
		t.Errorf("the forwarded unverified message says Verified=%q", got)
	}
}

// The destination is whatever the operator typed; it does not get to send the
// message, or the agent token, somewhere else.
func TestARedirectFromTheDestinationIsNotFollowed(t *testing.T) {
	h := newHookinlet(t)
	h.send(t, "one", "text/plain")
	h.send(t, "two", "text/plain")
	other := newDestination(t, nil)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect) //nolint:gosec // a test server redirecting to another test server
	}))
	defer redirecting.Close()
	r := relayFor(t, h, &destination{Server: redirecting}, filepath.Join(t.TempDir(), "state"))
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)
	if other.requests() != 0 {
		t.Errorf("the redirect target received %d requests", other.requests())
	}
}

func TestItNeedsAStateFile(t *testing.T) {
	h := newHookinlet(t)
	d := newDestination(t, nil)
	r := relayFor(t, h, d, "")
	if err := r.Run(t.Context()); err == nil {
		t.Error("a relay with no place to keep its position started")
	}
}

func TestAWrongTokenIsReportedAndRetriedNotSilentlySkipped(t *testing.T) {
	h := newHookinlet(t)
	h.send(t, "one", "text/plain")
	d := newDestination(t, nil)
	r := relayFor(t, h, d, filepath.Join(t.TempDir(), "state"))
	r.Token = "hki_wrong"
	r.Start = "now" // asking where the end is needs a good token
	if err := r.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("Run with a bad token: %v", err)
	}
	if d.requests() != 0 {
		t.Error("something was forwarded with a bad token")
	}
}
