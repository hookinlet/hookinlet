// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// ---- environment ----

var (
	// serverNow is the clock the server under test sees. Signed timestamps in
	// these tests are relative to it.
	serverNow = time.Unix(1700000010, 0).UTC()
	signedTS  = "1700000000"
)

type publishCall struct {
	id  inbox.ID
	seq int64
}

type recNotifier struct {
	mu    sync.Mutex
	calls []publishCall
	boom  bool
}

func (n *recNotifier) Publish(_ context.Context, id inbox.ID, seq int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, publishCall{id, seq})
	if n.boom {
		panic("notifier exploded")
	}
}

func (n *recNotifier) Subscribe(context.Context, inbox.ID) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

func (n *recNotifier) published() []publishCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.calls)
}

type afterCall struct {
	slug    string
	seq     int64
	created bool
}

type recHooks struct {
	inbox.NopHooks
	mu         sync.Mutex
	before     func(in *inbox.Inbox, size int64) error
	beforeSeen []string // "slug:size"
	afterSeen  []afterCall
	afterBoom  bool
}

func (h *recHooks) BeforeIngest(_ context.Context, in *inbox.Inbox, size int64) error {
	h.mu.Lock()
	h.beforeSeen = append(h.beforeSeen, fmt.Sprintf("%s:%d", in.Slug, size))
	f := h.before
	h.mu.Unlock()
	if f != nil {
		return f(in, size)
	}
	return nil
}

func (h *recHooks) AfterIngest(_ context.Context, in *inbox.Inbox, m *inbox.Message, created bool) {
	h.mu.Lock()
	h.afterSeen = append(h.afterSeen, afterCall{in.Slug, m.Seq, created})
	boom := h.afterBoom
	h.mu.Unlock()
	if boom {
		panic("hook exploded")
	}
}

func (h *recHooks) after() []afterCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.afterSeen)
}

func (h *recHooks) beforeCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.beforeSeen)
}

type env struct {
	t        *testing.T
	store    *sqlite.Store
	box      *secretbox.Box
	server   *Server
	notifier *recNotifier
	hooks    *recHooks
	logs     *syncBuffer
}

func newEnv(t *testing.T, mods ...func(*Options)) *env {
	t.Helper()
	store, err := sqlite.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	box, err := secretbox.New(bytes.Repeat([]byte{5}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, store: store, box: box, notifier: &recNotifier{}, hooks: &recHooks{}, logs: &syncBuffer{}}
	opts := Options{
		Addr:      "127.0.0.1:0",
		Logger:    slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Store:     store,
		SecretBox: box,
		Notifier:  e.notifier,
		Hooks:     e.hooks,
		Now:       func() time.Time { return serverNow },
	}
	for _, m := range mods {
		m(&opts)
	}
	if e.server, err = New(opts); err != nil {
		t.Fatal(err)
	}
	return e
}

// newInbox creates an inbox. secret, if non-empty, is sealed with the env's box.
func (e *env) newInbox(slug, verifier, secret string, mods ...func(*inbox.Inbox)) *inbox.Inbox {
	e.t.Helper()
	_, hash, err := token.Generate()
	if err != nil {
		e.t.Fatal(err)
	}
	in := &inbox.Inbox{Slug: slug, Name: slug, Verifier: verifier, AgentTokenHash: hash}
	if secret != "" {
		sealed, err := e.box.Seal([]byte(secret))
		if err != nil {
			e.t.Fatal(err)
		}
		in.VerifierSecret = sealed
	}
	for _, m := range mods {
		m(in)
	}
	if err := e.store.CreateInbox(e.t.Context(), in); err != nil {
		e.t.Fatal(err)
	}
	return in
}

type req struct {
	method  string
	path    string
	body    string
	headers http.Header
	remote  string
	length  *int64 // override Content-Length
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	e.t.Helper()
	if r.method == "" {
		r.method = http.MethodPost
	}
	hr := httptest.NewRequestWithContext(e.t.Context(), r.method, r.path, strings.NewReader(r.body))
	for k, vs := range r.headers {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	if r.length != nil {
		hr.ContentLength = *r.length
	}
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, hr)
	return rec
}

func (e *env) post(path, body string, kv ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return e.do(req{path: path, body: body, headers: h})
}

func (e *env) messages(in *inbox.Inbox) []*inbox.Message {
	e.t.Helper()
	zero := int64(0)
	page, err := e.store.Query(e.t.Context(), in.ID, inbox.Query{After: &zero, Limit: inbox.MaxQueryLimit})
	if err != nil {
		e.t.Fatal(err)
	}
	return page.Messages
}

func (e *env) count(in *inbox.Inbox) int { return len(e.messages(in)) }

func seqOf(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	var out struct {
		Seq int64 `json:"seq"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Seq == 0 {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	return out.Seq
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Error.Code == "" || out.Error.Message == "" {
		t.Fatalf("not a JSON error body: %q (%v)", rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("error Content-Type = %q", ct)
	}
	return out.Error.Code
}

func mac256(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

func hexMAC(secret, msg string) string { return hex.EncodeToString(mac256([]byte(secret), msg)) }

func githubSig(secret, body string) string {
	return "sha256=" + hex.EncodeToString(mac256([]byte(secret), body))
}

func stripeSig(secret, ts, body string) string {
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac256([]byte(secret), ts+"."+body))
}

// ---- method, routing, 404, 405, 413 ----

func TestIngestRejectsOtherMethods(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("methods", "none", "")
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodPatch, http.MethodOptions, "PROPFIND"} {
		for _, path := range []string{"/in/methods", "/in/methods/sub/path", "/in/does-not-exist"} {
			rec := e.do(req{method: method, path: path, body: "x"})
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d, want 405", method, path, rec.Code)
				continue
			}
			if got := rec.Header().Get("Allow"); got != "POST, PUT" {
				t.Errorf("%s %s: Allow = %q", method, path, got)
			}
			if method != http.MethodHead && errorCode(t, rec) != "method_not_allowed" {
				t.Errorf("%s %s: wrong error code", method, path)
			}
		}
	}
	if n := e.count(in); n != 0 {
		t.Errorf("%d messages stored by rejected methods", n)
	}
	if calls := e.hooks.beforeCalls(); len(calls) != 0 {
		t.Errorf("hooks ran for rejected methods: %v", calls)
	}
	// The method is checked before the inbox is looked up, so a wrong method
	// never reveals whether an inbox exists (same 405 either way, above).

	for _, method := range []string{http.MethodPost, http.MethodPut} {
		if rec := e.do(req{method: method, path: "/in/methods", body: "x"}); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, body %s", method, rec.Code, rec.Body.String())
		}
	}
}

func TestIngestNotFound(t *testing.T) {
	e := newEnv(t)
	off := e.newInbox("switched-off", "none", "", func(in *inbox.Inbox) { in.Disabled = true })
	e.newInbox("live-inbox", "none", "")

	var bodies []string
	for _, path := range []string{"/in/no-such-inbox", "/in/switched-off", "/in/switched-off/with/suffix", "/in/LIVE-INBOX", "/in/live-inbox%2Fx", "/in/%00"} {
		rec := e.post(path, "payload")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
			continue
		}
		if errorCode(t, rec) != "not_found" {
			t.Errorf("%s: wrong code", path)
		}
		bodies = append(bodies, rec.Body.String())
	}
	// An unknown key and a disabled inbox are indistinguishable to the caller.
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("404 bodies differ: %q vs %q", b, bodies[0])
		}
	}
	if n := e.count(off); n != 0 {
		t.Errorf("a disabled inbox stored %d messages", n)
	}
	if calls := e.hooks.beforeCalls(); len(calls) != 0 {
		t.Errorf("hooks ran for deliveries that found no inbox: %v", calls)
	}

	// Paths that are not ingest paths at all.
	for _, path := range []string{"/in", "/in/", "/", "/elsewhere"} {
		if rec := e.post(path, "x"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

func TestIngestBodyLimit(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("limited", "none", "", func(in *inbox.Inbox) { in.MaxBodyBytes = 100 })
	exact, over := strings.Repeat("a", 100), strings.Repeat("a", 101)

	// A body exactly at the limit is stored, byte for byte.
	if rec := e.post("/in/limited", exact); rec.Code != http.StatusOK {
		t.Fatalf("100 bytes: status %d", rec.Code)
	}
	if rec := e.post("/in/limited", ""); rec.Code != http.StatusOK {
		t.Fatalf("empty body: status %d", rec.Code)
	}
	msgs := e.messages(in)
	if len(msgs) != 2 || string(msgs[0].Body) != exact || len(msgs[1].Body) != 0 {
		t.Fatalf("stored %d messages with wrong bodies", len(msgs))
	}

	// One byte over is refused whether or not the sender declared a length.
	rec := e.post("/in/limited", over)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Errorf("declared length: status %d, body %s", rec.Code, rec.Body.String())
	}
	unknown := int64(-1)
	rec = e.do(req{path: "/in/limited", body: over, length: &unknown})
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Errorf("undeclared length: status %d, body %s", rec.Code, rec.Body.String())
	}
	// A false small Content-Length cannot smuggle a large body through.
	small := int64(5)
	rec = e.do(req{path: "/in/limited", body: strings.Repeat("a", 5000), length: &small})
	if rec.Code == http.StatusOK {
		t.Errorf("a body larger than its declared length was accepted")
	}
	// A huge declared length is refused without reading anything.
	huge := int64(1 << 40)
	rec = e.do(req{path: "/in/limited", body: "tiny", length: &huge})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge declared length: status %d", rec.Code)
	}

	if n := e.count(in); n != 2 {
		t.Errorf("%d messages stored, want only the two within the limit", n)
	}
	for _, c := range e.hooks.beforeCalls() {
		if strings.HasSuffix(c, ":101") || strings.HasSuffix(c, ":5000") {
			t.Errorf("hook saw an oversized delivery: %s", c)
		}
	}
}

func TestIngestTreatsThePathAndQueryAsData(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("paths", "none", "")
	for _, path := range []string{"/in/paths", "/in/paths/", "/in/paths/a/b/c", "/in/paths/a%2Fb", "/in/paths/x?y=1&z=%20"} {
		if rec := e.post(path, "x"); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
	}
	var got [][2]string
	for _, m := range e.messages(in) {
		got = append(got, [2]string{m.PathSuffix, m.Query})
	}
	want := [][2]string{{"", ""}, {"", ""}, {"a/b/c", ""}, {"a/b", ""}, {"x", "y=1&z=%20"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("path suffixes and queries = %q, want %q", got, want)
	}
}

// ---- what gets stored ----

func TestIngestStoresTheDeliveryExactly(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("exact", "none", "")
	body := string([]byte{0xff, 0xfe, 0x00, 'h', 'i', 0x80, '\r', '\n'})

	rec := e.do(req{
		method: http.MethodPut, path: "/in/exact/orders/42?x=1", body: body, remote: "203.0.113.9:5555",
		headers: http.Header{
			"Content-Type":  {"application/octet-stream"},
			"X-Custom":      {"a", "b"},
			"Authorization": {"Bearer must-not-be-stored"},
			"Cookie":        {"session=must-not-be-stored"},
		},
	})
	if seq := seqOf(t, rec); seq != 1 {
		t.Fatalf("seq = %d", seq)
	}

	m := e.messages(in)[0]
	if m.Method != "PUT" || m.PathSuffix != "orders/42" || m.Query != "x=1" || m.ContentType != "application/octet-stream" ||
		string(m.Body) != body || !m.Verified || m.SourceIP != "203.0.113.9" {
		t.Errorf("stored message differs: %+v", *m)
	}
	if !reflect.DeepEqual(m.Headers["X-Custom"], []string{"a", "b"}) {
		t.Errorf("X-Custom = %v", m.Headers["X-Custom"])
	}
	for k := range m.Headers {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Cookie") {
			t.Errorf("header %s was persisted", k)
		}
	}
	if raw, _ := json.Marshal(m.Headers); bytes.Contains(raw, []byte("must-not-be-stored")) {
		t.Errorf("a secret header value was persisted: %s", raw)
	}

	// The response comes only after the message is in the Store: it is already
	// readable when the call returns.
	if seq, _ := e.store.LatestSeq(t.Context(), in.ID); seq != 1 {
		t.Errorf("LatestSeq = %d at the time of the response", seq)
	}
	if rec := e.post("/in/exact", "second"); seqOf(t, rec) != 2 {
		t.Error("second delivery did not get seq 2")
	}
}

// ---- verification ----

func TestIngestVerifiers(t *testing.T) {
	t.Run("none accepts anything", func(t *testing.T) {
		e := newEnv(t)
		e.newInbox("open", "none", "")
		if rec := e.post("/in/open", "x"); rec.Code != http.StatusOK {
			t.Errorf("status %d", rec.Code)
		}
	})

	t.Run("token", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("tok", "token", "s3cret-token")
		if rec := e.post("/in/tok", "x", "X-Inbox-Token", "s3cret-token", "X-Other", "kept"); rec.Code != http.StatusOK {
			t.Fatalf("valid token: status %d, %s", rec.Code, rec.Body.String())
		}
		for name, hdrs := range map[string][]string{
			"wrong": {"X-Inbox-Token", "nope"}, "missing": {}, "empty": {"X-Inbox-Token", ""},
		} {
			rec := e.post("/in/tok", "x", hdrs...)
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "verification_failed" {
				t.Errorf("%s token: status %d, %s", name, rec.Code, rec.Body.String())
			}
		}
		msgs := e.messages(in)
		if len(msgs) != 1 {
			t.Fatalf("%d stored, want 1", len(msgs))
		}
		for k, vs := range msgs[0].Headers {
			if strings.EqualFold(k, "X-Inbox-Token") {
				t.Errorf("the token header was persisted: %v", vs)
			}
		}
		if msgs[0].Headers.Get("X-Other") != "kept" {
			t.Error("an ordinary header was lost")
		}
	})

	t.Run("custom token header is also kept out of storage", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("tok2", "token", "abc", func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`{"header":"X-Secret-Key"}`) })
		if rec := e.post("/in/tok2", "x", "X-Secret-Key", "abc"); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if h := e.messages(in)[0].Headers; h.Get("X-Secret-Key") != "" {
			t.Errorf("custom token header persisted: %v", h)
		}
	})

	t.Run("github", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("gh-box", "github", "gh-secret", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailReject })
		body := `{"zen":"keep it logically awesome"}`
		rec := e.post("/in/gh-box", body, "X-Hub-Signature-256", githubSig("gh-secret", body), "X-GitHub-Delivery", "d-1", "X-GitHub-Event", "ping")
		if rec.Code != http.StatusOK {
			t.Fatalf("valid: status %d, %s", rec.Code, rec.Body.String())
		}
		m := e.messages(in)[0]
		if !m.Verified || m.DeliveryID != "d-1" || m.EventType != "ping" {
			t.Errorf("stored %+v", *m)
		}
		bad := e.post("/in/gh-box", body, "X-Hub-Signature-256", githubSig("other-secret", body))
		if bad.Code != http.StatusUnauthorized || errorCode(t, bad) != "verification_failed" || !strings.Contains(e.logs.String(), "signature mismatch") {
			t.Errorf("wrong secret: status %d, %s", bad.Code, bad.Body.String())
		}
		if tamper := e.post("/in/gh-box", body+" ", "X-Hub-Signature-256", githubSig("gh-secret", body)); tamper.Code != http.StatusUnauthorized {
			t.Errorf("tampered body: status %d", tamper.Code)
		}
		if missing := e.post("/in/gh-box", body); missing.Code != http.StatusUnauthorized {
			t.Errorf("missing header: status %d", missing.Code)
		}
		if n := e.count(in); n != 1 {
			t.Errorf("%d stored, want only the valid one", n)
		}
	})

	t.Run("stripe", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("st-box", "stripe", "whsec_server")
		body := `{"id":"evt_9","type":"invoice.paid"}`
		if rec := e.post("/in/st-box", body, "Stripe-Signature", stripeSig("whsec_server", signedTS, body)); rec.Code != http.StatusOK {
			t.Fatalf("valid: status %d, %s", rec.Code, rec.Body.String())
		}
		m := e.messages(in)[0]
		if m.DeliveryID != "evt_9" || m.EventType != "invoice.paid" || !m.Verified {
			t.Errorf("stored %+v", *m)
		}
		stale := e.post("/in/st-box", body, "Stripe-Signature", stripeSig("whsec_server", "1699990000", body))
		if stale.Code != http.StatusUnauthorized || !strings.Contains(e.logs.String(), "timestamp outside tolerance") {
			t.Errorf("stale: status %d, %s", stale.Code, stale.Body.String())
		}
		if n := e.count(in); n != 1 {
			t.Errorf("%d stored", n)
		}
	})

	t.Run("slack answers the handshake without storing it", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("sl-box", "slack", "slack-secret")
		sign := func(secret, body string) string {
			return "v0=" + hex.EncodeToString(mac256([]byte(secret), "v0:"+signedTS+":"+body))
		}
		challenge := `{"type":"url_verification","challenge":"abc123-challenge"}`
		rec := e.post("/in/sl-box", challenge, "X-Slack-Request-Timestamp", signedTS, "X-Slack-Signature", sign("slack-secret", challenge))
		if rec.Code != http.StatusOK || rec.Body.String() != "abc123-challenge" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("handshake: status %d, body %q, type %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
		}
		forged := e.post("/in/sl-box", challenge, "X-Slack-Request-Timestamp", signedTS, "X-Slack-Signature", sign("wrong", challenge))
		if forged.Code != http.StatusUnauthorized || strings.Contains(forged.Body.String(), "abc123-challenge") {
			t.Errorf("a forged handshake: status %d, body %s", forged.Code, forged.Body.String())
		}
		if n := e.count(in); n != 0 {
			t.Errorf("%d messages stored for handshakes", n)
		}
		if calls := e.hooks.after(); len(calls) != 0 || len(e.notifier.published()) != 0 {
			t.Errorf("a handshake triggered post-commit callbacks: %v %v", calls, e.notifier.published())
		}

		event := `{"type":"event_callback","event_id":"Ev123","event":{"type":"message"}}`
		if rec := e.post("/in/sl-box", event, "X-Slack-Request-Timestamp", signedTS, "X-Slack-Signature", sign("slack-secret", event)); rec.Code != http.StatusOK {
			t.Fatalf("event: status %d, %s", rec.Code, rec.Body.String())
		}
		if m := e.messages(in)[0]; m.EventType != "message" || m.DeliveryID != "Ev123" {
			t.Errorf("stored %+v", *m)
		}
	})

	t.Run("generic hmac", func(t *testing.T) {
		e := newEnv(t)
		cfg := `{"header":"X-Signature","prefix":"sha256=","delivery_id_header":"X-Request-Id"}`
		in := e.newInbox("gen", "hmac", "generic-secret", func(in *inbox.Inbox) {
			in.VerifierConfig = json.RawMessage(cfg)
			in.EventTypePointer = "/kind"
		})
		body := `{"kind":"order.created"}`
		rec := e.post("/in/gen", body, "X-Signature", githubSig("generic-secret", body), "X-Request-Id", "rq-1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, %s", rec.Code, rec.Body.String())
		}
		if m := e.messages(in)[0]; m.DeliveryID != "rq-1" || m.EventType != "order.created" {
			t.Errorf("stored %+v", *m)
		}
	})
}

func TestIngestServerSideProblemsAreNotReportedAsBadSignatures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
		mods  []func(*Options)
	}{
		{"unknown verifier name", func(e *env) { e.newInbox("broken", "carrier-pigeon", "") }, nil},
		{"verifier needs a secret the inbox lacks", func(e *env) { e.newInbox("broken", "github", "") }, nil},
		{"verifier config is invalid", func(e *env) {
			e.newInbox("broken", "stripe", "s", func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`{"tolerance_seconds":-5}`) })
		}, nil},
		{"secret sealed under a different master key", func(e *env) {
			other, _ := secretbox.New(bytes.Repeat([]byte{99}, secretbox.KeySize))
			sealed, _ := other.Seal([]byte("secret"))
			e.newInbox("broken", "github", "", func(in *inbox.Inbox) { in.VerifierSecret = sealed })
		}, nil},
		{"inbox has a secret but the server has no SecretBox", func(e *env) { e.newInbox("broken", "github", "secret") },
			[]func(*Options){func(o *Options) { o.SecretBox = nil }}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, c.mods...)
			c.setup(e)
			// Even on an inbox that stores unverified deliveries, a server-side
			// problem must not be recorded as a bad signature or acknowledged.
			rec := e.post("/in/broken", "x", "X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
			if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != "internal" {
				t.Fatalf("status %d, %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "verifier") {
				t.Errorf("the response explains the server's configuration to the sender: %s", rec.Body.String())
			}
			in, _ := e.store.GetInboxBySlug(t.Context(), "broken")
			if n := e.count(in); n != 0 {
				t.Errorf("%d messages stored", n)
			}
			if !strings.Contains(e.logs.String(), `"level":"ERROR"`) {
				t.Errorf("the cause was not logged: %s", e.logs.String())
			}
		})
	}
}

func TestVerifyFailModes(t *testing.T) {
	body := `{"event":"x"}`
	t.Run("reject answers 401 and stores nothing", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("rej", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailReject })
		rec := e.post("/in/rej", body, "X-Hub-Signature-256", githubSig("wrong", body))
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "verification_failed" {
			t.Fatalf("status %d, %s", rec.Code, rec.Body.String())
		}
		if e.count(in) != 0 || len(e.hooks.after()) != 0 || len(e.notifier.published()) != 0 {
			t.Error("a rejected delivery left a trace")
		}
	})

	t.Run("store keeps it, marked unverified, and trusts nothing it claims", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("keep", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
		rec := e.post("/in/keep", body, "X-Hub-Signature-256", githubSig("wrong", body), "X-GitHub-Delivery", "forged-id", "X-GitHub-Event", "forged")
		seq := seqOf(t, rec)
		m := e.messages(in)[0]
		if m.Seq != seq || m.Verified || m.VerifyError != "signature mismatch" {
			t.Errorf("stored %+v", *m)
		}
		if m.DeliveryID != "" || m.EventType != "" {
			t.Errorf("claims from an unverified delivery were kept: id %q type %q", m.DeliveryID, m.EventType)
		}
		if got := e.hooks.after(); len(got) != 1 || !got[0].created {
			t.Errorf("AfterIngest calls = %v", got)
		}
	})

	t.Run("a stored forgery cannot suppress the genuine delivery", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("forgery", "stripe", "whsec_f", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
		real := `{"id":"evt_real","type":"charge.succeeded"}`
		// The attacker posts first, claiming the id the genuine event will carry.
		forged := e.post("/in/forgery", real, "Stripe-Signature", stripeSig("attacker-guess", signedTS, real))
		forgedSeq := seqOf(t, forged)
		genuine := e.post("/in/forgery", real, "Stripe-Signature", stripeSig("whsec_f", signedTS, real))
		genuineSeq := seqOf(t, genuine)
		if genuineSeq == forgedSeq {
			t.Fatal("the genuine delivery was treated as a duplicate of the forgery")
		}
		msgs := e.messages(in)
		if len(msgs) != 2 || msgs[0].Verified || !msgs[1].Verified || msgs[1].DeliveryID != "evt_real" {
			t.Errorf("stored %d messages: %+v", len(msgs), msgs)
		}
	})
}

// ---- duplicates ----

func TestDuplicateDeliveries(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("dups", "github", "s")
	send := func(body, delivery string) *httptest.ResponseRecorder {
		return e.post("/in/dups", body, "X-Hub-Signature-256", githubSig("s", body), "X-GitHub-Delivery", delivery)
	}

	first := seqOf(t, send(`{"n":1}`, "delivery-A"))
	again := seqOf(t, send(`{"n":1}`, "delivery-A"))
	third := seqOf(t, send(`{"n":1,"retry":true}`, "delivery-A")) // GitHub may redeliver with identical id
	if again != first || third != first {
		t.Fatalf("a duplicate got seq %d and %d, want the original %d", again, third, first)
	}
	if n := e.count(in); n != 1 {
		t.Fatalf("%d rows for one delivery", n)
	}
	other := seqOf(t, send(`{"n":2}`, "delivery-B"))
	if other != first+1 {
		t.Errorf("a different delivery got seq %d, want %d", other, first+1)
	}
	if m := e.messages(in)[0]; string(m.Body) != `{"n":1}` {
		t.Errorf("the original was overwritten: %s", m.Body)
	}

	if got := e.hooks.after(); !reflect.DeepEqual(got, []afterCall{
		{"dups", 1, true}, {"dups", 1, false}, {"dups", 1, false}, {"dups", 2, true},
	}) {
		t.Errorf("AfterIngest calls = %v", got)
	}
	// Only newly stored messages are announced.
	if got := e.notifier.published(); len(got) != 2 || got[0].seq != 1 || got[1].seq != 2 {
		t.Errorf("Publish calls = %v, want one per new message", got)
	}
}

func TestConcurrentDuplicatesStoreOnce(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("race", "github", "s")
	body := `{"n":1}`
	var wg sync.WaitGroup
	seqs := make(chan int64, 30)
	for range 30 {
		wg.Go(func() {
			rec := e.post("/in/race", body, "X-Hub-Signature-256", githubSig("s", body), "X-GitHub-Delivery", "same")
			if rec.Code != http.StatusOK {
				t.Errorf("status %d", rec.Code)
				return
			}
			var out struct{ Seq int64 }
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			seqs <- out.Seq
		})
	}
	wg.Wait()
	close(seqs)
	for s := range seqs {
		if s != 1 {
			t.Errorf("a racing duplicate got seq %d", s)
		}
	}
	if n := e.count(in); n != 1 {
		t.Errorf("%d rows for one delivery", n)
	}
	created := 0
	for _, c := range e.hooks.after() {
		if c.created {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d AfterIngest calls reported created", created)
	}
}

// ---- hooks ----

func TestHooksCanRejectDeliveries(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		want string
	}{
		{"rate limited", inbox.ErrRateLimited, http.StatusTooManyRequests, "rate_limited"},
		{"quota exceeded", inbox.ErrQuotaExceeded, http.StatusPaymentRequired, "quota_exceeded"},
		{"forbidden", inbox.ErrForbidden, http.StatusForbidden, "forbidden"},
		{"wrapped rate limit", fmt.Errorf("plan limit: %w", inbox.ErrRateLimited), http.StatusTooManyRequests, "rate_limited"},
		{"wrapped quota", fmt.Errorf("monthly cap: %w", inbox.ErrQuotaExceeded), http.StatusPaymentRequired, "quota_exceeded"},
		{"unknown error", errors.New("db is on fire: password=hunter2"), http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			in := e.newInbox("gated", "github", "s")
			e.hooks.before = func(*inbox.Inbox, int64) error { return c.err }
			body := `{"x":1}`

			// The hook runs before verification, so a delivery with a bad
			// signature is still refused by the hook first.
			rec := e.post("/in/gated", body, "X-Hub-Signature-256", githubSig("wrong", body))
			if rec.Code != c.code || errorCode(t, rec) != c.want {
				t.Fatalf("status %d, %s; want %d %s", rec.Code, rec.Body.String(), c.code, c.want)
			}
			if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "on fire") {
				t.Errorf("an internal error text reached the client: %s", rec.Body.String())
			}
			if e.count(in) != 0 {
				t.Error("a rejected delivery was stored")
			}
			if len(e.hooks.after()) != 0 || len(e.notifier.published()) != 0 {
				t.Error("post-commit callbacks ran for a rejected delivery")
			}
			// A valid delivery is refused just the same.
			valid := e.post("/in/gated", body, "X-Hub-Signature-256", githubSig("s", body))
			if valid.Code != c.code {
				t.Errorf("valid delivery: status %d, want %d", valid.Code, c.code)
			}
			if e.count(in) != 0 {
				t.Error("a rejected valid delivery was stored")
			}
		})
	}
}

func TestHooksSeeTheInboxAndBodySize(t *testing.T) {
	e := newEnv(t)
	e.newInbox("sized", "none", "")
	e.post("/in/sized", strings.Repeat("z", 1234))
	e.post("/in/sized", "")
	if got := e.hooks.beforeCalls(); !reflect.DeepEqual(got, []string{"sized:1234", "sized:0"}) {
		t.Errorf("BeforeIngest saw %v", got)
	}
}

func TestPostCommitFailuresDoNotUndoAnAcknowledgedDelivery(t *testing.T) {
	t.Run("AfterIngest panics", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("boom-hook", "none", "")
		e.hooks.afterBoom = true
		rec := e.post("/in/boom-hook", "x")
		if seqOf(t, rec) != 1 || e.count(in) != 1 {
			t.Error("a panicking hook turned a stored delivery into an error")
		}
		if !strings.Contains(e.logs.String(), "panic after commit") {
			t.Error("the panic was not logged")
		}
	})
	t.Run("Publish panics", func(t *testing.T) {
		e := newEnv(t)
		in := e.newInbox("boom-notify", "none", "")
		e.notifier.boom = true
		rec := e.post("/in/boom-notify", "x")
		if seqOf(t, rec) != 1 || e.count(in) != 1 {
			t.Error("a panicking notifier turned a stored delivery into an error")
		}
	})
}

func TestNotifierPublishedAfterCommit(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("announce", "none", "")
	// By the time Publish runs, the message must already be readable.
	var seenInStore bool
	e.server.opts.Notifier = notifierFunc(func(ctx context.Context, id inbox.ID, seq int64) {
		_, err := e.store.Get(ctx, id, seq)
		seenInStore = err == nil
	})
	e.post("/in/announce", "x")
	if !seenInStore {
		t.Error("Publish ran before the message was committed")
	}
	_ = in
}

type notifierFunc func(ctx context.Context, id inbox.ID, seq int64)

func (f notifierFunc) Publish(ctx context.Context, id inbox.ID, seq int64) { f(ctx, id, seq) }
func (f notifierFunc) Subscribe(context.Context, inbox.ID) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

// ---- event type, source ip, logging, readiness ----

func TestEventTypePointer(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("typed", "none", "", func(in *inbox.Inbox) { in.EventTypePointer = "/meta/type" })
	for _, body := range []string{
		`{"meta":{"type":"user.created"}}`,                     // found
		`{"meta":{"kind":"user.created"}}`,                     // missing
		`{"meta":{"type":42}}`,                                 // not a string
		`{"meta":{"type":""}}`,                                 // empty
		`{"meta":{"type":"` + strings.Repeat("x", 300) + `"}}`, // too long
		`not json at all`,
		``,
	} {
		e.post("/in/typed", body)
	}
	var got []string
	for _, m := range e.messages(in) {
		got = append(got, m.EventType)
	}
	if want := []string{"user.created", "", "", "", "", "", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("event types = %q, want %q", got, want)
	}
}

func TestEventTypeFromAPresetBeatsThePointer(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("both", "github", "s", func(in *inbox.Inbox) { in.EventTypePointer = "/type" })
	body := `{"type":"from-body"}`
	e.post("/in/both", body, "X-Hub-Signature-256", githubSig("s", body), "X-GitHub-Event", "from-header")
	if got := e.messages(in)[0].EventType; got != "from-header" {
		t.Errorf("event type = %q", got)
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("fd00::/8")}
	tests := []struct {
		name, remote string
		xff          []string
		trusted      []netip.Prefix
		want         string
	}{
		{"direct peer", "198.51.100.7:4000", nil, nil, "198.51.100.7"},
		{"header ignored without trusted proxies", "198.51.100.7:4000", []string{"1.2.3.4"}, nil, "198.51.100.7"},
		{"header ignored from an untrusted peer", "198.51.100.7:4000", []string{"1.2.3.4"}, trusted, "198.51.100.7"},
		{"one trusted proxy", "10.1.2.3:4000", []string{"203.0.113.5"}, trusted, "203.0.113.5"},
		{"two trusted proxies", "10.1.2.3:4000", []string{"203.0.113.5, 10.9.9.9"}, trusted, "203.0.113.5"},
		{"header split across lines", "10.1.2.3:4000", []string{"203.0.113.5", "10.9.9.9"}, trusted, "203.0.113.5"},
		{"client-supplied entries to the left are ignored", "10.1.2.3:4000", []string{"6.6.6.6, 203.0.113.5"}, trusted, "203.0.113.5"},
		{"a spoofed trusted address on the left does not help", "10.1.2.3:4000", []string{"10.5.5.5, 203.0.113.5"}, trusted, "203.0.113.5"},
		{"every hop trusted", "10.1.2.3:4000", []string{"10.5.5.5, 192.168.1.9"}, trusted, "10.1.2.3"},
		{"trusted peer without a header", "10.1.2.3:4000", nil, trusted, "10.1.2.3"},
		{"garbage in the header", "10.1.2.3:4000", []string{"not-an-ip"}, trusted, "10.1.2.3"},
		{"garbage after a valid hop", "10.1.2.3:4000", []string{"203.0.113.5, garbage"}, trusted, "10.1.2.3"},
		{"empty header", "10.1.2.3:4000", []string{""}, trusted, "10.1.2.3"},
		{"ipv6 client behind a trusted proxy", "10.1.2.3:4000", []string{"2001:db8::1"}, trusted, "2001:db8::1"},
		{"ipv6 trusted peer", "[fd00::1]:4000", []string{"2001:db8::2"}, trusted, "2001:db8::2"},
		{"ipv4-mapped peer", "[::ffff:10.1.2.3]:4000", []string{"203.0.113.5"}, trusted, "203.0.113.5"},
		{"unparseable peer", "weird", []string{"203.0.113.5"}, trusted, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/in/x", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r, tt.trusted); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIngestUsesTrustedProxies(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} })
	in := e.newInbox("proxied", "none", "")
	e.do(req{path: "/in/proxied", body: "x", remote: "10.0.0.1:1", headers: http.Header{"X-Forwarded-For": {"198.51.100.20"}}})
	e.do(req{path: "/in/proxied", body: "x", remote: "198.51.100.99:1", headers: http.Header{"X-Forwarded-For": {"1.1.1.1"}}})
	var ips []string
	for _, m := range e.messages(in) {
		ips = append(ips, m.SourceIP)
	}
	if !reflect.DeepEqual(ips, []string{"198.51.100.20", "198.51.100.99"}) {
		t.Errorf("source ips = %v", ips)
	}
}

func TestIngestNeverLogsSecretsOrBodies(t *testing.T) {
	e := newEnv(t)
	e.newInbox("quiet", "token", "the-shared-secret-value")
	e.newInbox("quiet-gh", "github", "github-secret-value")
	body := "BODY-MARKER-d41d8cd9"

	e.post("/in/quiet", body, "X-Inbox-Token", "the-shared-secret-value", "Authorization", "Bearer AUTH-MARKER-77", "Cookie", "COOKIE-MARKER-88")
	e.post("/in/quiet", body, "X-Inbox-Token", "wrong-token-MARKER-99")
	sig := githubSig("github-secret-value", body)
	e.post("/in/quiet-gh", body, "X-Hub-Signature-256", sig)
	e.post("/in/quiet-gh", body, "X-Hub-Signature-256", githubSig("bad", body))
	e.post("/in/does-not-exist-MARKER-key", body)

	logs := e.logs.String()
	for _, secret := range []string{
		"the-shared-secret-value", "github-secret-value", "BODY-MARKER", "AUTH-MARKER", "COOKIE-MARKER",
		"wrong-token-MARKER", strings.TrimPrefix(sig, "sha256="), "does-not-exist-MARKER-key",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain %q", secret)
		}
	}
	if !strings.Contains(logs, `"slug":"quiet"`) || !strings.Contains(logs, `"seq":1`) {
		t.Errorf("the logs lack the expected slug and seq: %s", logs)
	}
}

func TestReadyz(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	_ = e.store.Close()
	rec := e.do(req{method: http.MethodGet, path: "/readyz"})
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "not_ready" {
		t.Errorf("after the store closed: status %d, %s", rec.Code, rec.Body.String())
	}
	// Liveness is independent of dependencies.
	if rec := e.do(req{method: http.MethodGet, path: "/healthz"}); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d", rec.Code)
	}
}

func TestManyConcurrentDeliveries(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("busy", "token", "k")
	const n = 100
	var wg sync.WaitGroup
	seqs := make(chan int64, n)
	for i := range n {
		wg.Go(func() {
			rec := e.post("/in/busy", fmt.Sprintf("delivery-%d", i), "X-Inbox-Token", "k")
			var out struct{ Seq int64 }
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
				t.Errorf("delivery %d: status %d", i, rec.Code)
				return
			}
			seqs <- out.Seq
		})
	}
	wg.Wait()
	close(seqs)
	var got []int64
	for s := range seqs {
		got = append(got, s)
	}
	slices.Sort(got)
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("seqs are not exactly 1..%d once each: %v", n, got)
		}
	}
	if len(got) != n || e.count(in) != n {
		t.Errorf("%d acknowledged, %d stored, want %d", len(got), e.count(in), n)
	}
}

func TestRequestBodyReadErrorsAreNotServerErrors(t *testing.T) {
	e := newEnv(t)
	in := e.newInbox("aborted", "none", "")
	hr := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/in/aborted", io.NopCloser(errReader{}))
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, hr)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "bad_request" {
		t.Errorf("status %d, %s", rec.Code, rec.Body.String())
	}
	if e.count(in) != 0 {
		t.Error("a delivery that failed mid-read was stored")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }
