// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// With on_verify_fail=store an unverified delivery is kept, but nothing about it
// may be trusted, including an event type that an inbox's JSON pointer would
// lift out of the (attacker-controlled) body.
func TestUnverifiedDeliveriesGetNoEventTypeFromThePointer(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("forged-type", "github", "s", func(in *inbox.Inbox) {
		in.OnVerifyFail = inbox.OnVerifyFailStore
		in.EventTypePointer = "/action"
	})
	body := `{"action":"deploy.approved"}`

	forged := e.post("/in/forged-type", body, "X-Hub-Signature-256", githubSig("wrong", body))
	genuine := e.post("/in/forged-type", body, "X-Hub-Signature-256", githubSig("s", body))
	if forged.Code != http.StatusOK || genuine.Code != http.StatusOK {
		t.Fatalf("statuses %d and %d", forged.Code, genuine.Code)
	}
	msgs := e.messages(in)
	if len(msgs) != 2 {
		t.Fatalf("%d messages", len(msgs))
	}
	if msgs[0].Verified || msgs[0].EventType != "" {
		t.Errorf("a forged delivery got event type %q (verified=%v)", msgs[0].EventType, msgs[0].Verified)
	}
	if !msgs[1].Verified || msgs[1].EventType != "deploy.approved" {
		t.Errorf("the genuine delivery has type %q (verified=%v)", msgs[1].EventType, msgs[1].Verified)
	}
}

// A sender that trickles a body must be cut off, or anyone holding an ingest key
// can hold connections and buffers open indefinitely.
func TestSlowBodiesAreCutOff(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ReadBodyTimeout = 200 * time.Millisecond })
	in := e.newInbox("slow-box", "trusted", "")
	ts := httptest.NewServer(e.server.Handler())
	defer ts.Close()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Promise 100 bytes, send one, then stall.
	if _, err := conn.Write([]byte("POST /in/slow-box HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nx")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response to a stalled body within 5s: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the stalled body was cut off after %v", d)
	}
	if e.count(in) != 0 {
		t.Error("a truncated delivery was stored")
	}
}

func TestSlackChallengeResponseIsConstrained(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("slack-box", "slack", "slack-secret")
	sign := func(body string) string {
		return "v0=" + hexMAC("slack-secret", "v0:"+signedTS+":"+body)
	}
	body := `{"type":"url_verification","challenge":"<script>alert(1)</script>"}`
	rec := e.post("/in/slack-box", body, "X-Slack-Request-Timestamp", signedTS, "X-Slack-Signature", sign(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("the echoed challenge can be sniffed as HTML: %v", rec.Header())
	}

	// An oversized challenge is not reflected at all; it is stored like any message.
	long := `{"type":"url_verification","challenge":"` + strings.Repeat("c", 400) + `"}`
	rec = e.post("/in/slack-box", long, "X-Slack-Request-Timestamp", signedTS, "X-Slack-Signature", sign(long))
	if strings.Contains(rec.Body.String(), "cccc") {
		t.Errorf("an oversized challenge was echoed: %q", rec.Body.String())
	}
	if e.count(in) != 1 {
		t.Errorf("%d stored, want the oversized handshake kept as an ordinary message", e.count(in))
	}
}

// A 401 tells the sender that verification failed. It must not describe the
// inbox's configuration; the detail goes to the server log.
func TestVerificationFailureResponseIsGeneric(t *testing.T) {
	e := newEnv(t)
	e.newInbox("quiet-fail", "hmac", "s", func(in *inbox.Inbox) {
		in.VerifierConfig = json.RawMessage(`{"header":"X-Acme-Private-Sig"}`)
	})
	rec := e.post("/in/quiet-fail", "x")
	if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "verification_failed" {
		t.Fatalf("status %d, %s", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{"X-Acme-Private-Sig", "missing", "header", "mismatch", "hmac"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the response reveals configuration (%q): %s", leak, rec.Body.String())
		}
	}
	if !strings.Contains(e.logs.String(), "missing X-Acme-Private-Sig header") {
		t.Errorf("the detailed reason is not in the server log: %s", e.logs.String())
	}
}

// If a verifier panics, the decrypted secret must still be wiped.
func TestSecretIsWipedEvenWhenTheVerifierPanics(t *testing.T) {
	var captured []byte
	reg := verify.NewRegistry()
	if err := reg.Register("spy", func(json.RawMessage) (verify.Verifier, error) { return spyVerifier{&captured}, nil }); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options) { o.Verifiers = reg })
	e.newInbox("spy-box", "spy", "top-secret-value")

	rec := e.post("/in/spy-box", "x")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if len(captured) == 0 {
		t.Fatal("the spy never saw the secret")
	}
	for i, b := range captured {
		if b != 0 {
			t.Fatalf("byte %d of the decrypted secret was not wiped: %q", i, captured)
		}
	}
}

type spyVerifier struct{ seen *[]byte }

func (v spyVerifier) Verify(_ *http.Request, _, secret []byte, _ time.Time) (verify.Result, error) {
	*v.seen = secret
	panic("verifier exploded")
}
