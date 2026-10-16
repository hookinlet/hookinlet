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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// The fuzz targets here run their seeds in a normal `go test`. To fuzz one:
//
//	go test -run='^$' -fuzz=FuzzIngest -fuzztime=90s ./pkg/server
//
// They drive the real handler (routing, parameter parsing, verifiers, store)
// with hostile input, and fail on a panic, a 5xx, a hang, or an answer that
// breaks the API's own promises.

const fuzzAdminToken = "fuzz-admin-token-0123456789-abcdefghij"

// fuzzLog is where the server under test logs; a test that needs to see why a
// request failed can point it at os.Stderr.
var fuzzLog io.Writer = io.Discard

var fuzzNow = time.Unix(1_800_000_000, 0).UTC()

type fuzzEnv struct {
	h     http.Handler
	store *sqlite.Store
	ins   map[string]*inbox.Inbox
}

// fuzzSecrets are the verifier secrets of the inboxes made by newFuzzEnv.
var fuzzSecrets = map[string]string{
	"fz-github": "gh-secret",
	"fz-token":  "tok-secret",
	"fz-stripe": "whsec_stripe",
	"fz-slack":  "slack-secret",
	"fz-hmac":   "hmac-secret",
}

// newFuzzEnv builds a server with one inbox per verifier. It takes a TB so it
// can be built once, before the fuzzer starts, and not for every input.
func newFuzzEnv(tb testing.TB) *fuzzEnv {
	tb.Helper()
	store, err := sqlite.Open(context.Background(), tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	box, err := secretbox.New(bytes.Repeat([]byte{5}, secretbox.KeySize))
	if err != nil {
		tb.Fatal(err)
	}
	s, err := New(Options{
		Addr: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(fuzzLog, nil)), Store: store, SecretBox: box,
		Authenticator: token.NewAuthenticator(store, fuzzAdminToken),
		Now:           func() time.Time { return fuzzNow },
	})
	if err != nil {
		tb.Fatal(err)
	}
	e := &fuzzEnv{h: s.Handler(), store: store, ins: map[string]*inbox.Inbox{}}
	for slug, verifier := range map[string]string{
		"fz-none": "none", "fz-github": "github", "fz-token": "token", "fz-stripe": "stripe",
		"fz-slack": "slack", "fz-hmac": "hmac", "fz-sw": "standardwebhooks",
	} {
		uid, err := inbox.NewUID()
		if err != nil {
			tb.Fatal(err)
		}
		in := &inbox.Inbox{Slug: slug, UID: uid, IngestKey: slug, Name: slug, Verifier: verifier, EventTypePointer: "/type"}
		secret := fuzzSecrets[slug]
		if slug == "fz-sw" {
			secret = "whsec_c3dvcmQ=" //nolint:gosec // a test secret
		}
		if secret != "" {
			if in.VerifierSecret, err = box.Seal([]byte(secret), inbox.VerifierSecretContext(uid)); err != nil {
				tb.Fatal(err)
			}
		}
		if slug == "fz-hmac" {
			in.VerifierConfig = json.RawMessage(`{"header":"X-Sig","timestamp_header":"X-Ts","delivery_id_header":"X-Id"}`)
		}
		if slug == "fz-stripe" {
			in.OnVerifyFail = inbox.OnVerifyFailStore
		}
		if err := store.CreateInbox(context.Background(), in); err != nil {
			tb.Fatal(err)
		}
		e.ins[slug] = in
	}
	for _, body := range []string{`{"type":"a.created","n":1}`, `plain`, "\xff\xfe", `[1,2]`} {
		e.ingest(tb, "fz-none", body)
	}
	return e
}

func (e *fuzzEnv) ingest(tb testing.TB, key, body string) {
	tb.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/in/"+key, strings.NewReader(body))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		tb.Fatalf("seeding %s: status %d: %s", key, rec.Code, rec.Body)
	}
}

// serve runs a request against the handler with a deadline, so a handler that
// hangs (or a stream that never ends) shows up as a slow, bounded exec. It
// reports false when the deadline passed before the handler answered with
// something other than a server error: a 5xx after the client has gone is not
// a finding.
func (e *fuzzEnv) serve(r *http.Request, limit time.Duration) (rec *httptest.ResponseRecorder, live bool) {
	ctx, cancel := context.WithTimeout(r.Context(), limit)
	defer cancel()
	rec = httptest.NewRecorder()
	start := time.Now()
	e.h.ServeHTTP(rec, r.WithContext(ctx))
	if d := time.Since(start); d > limit+2*time.Second {
		panic("the handler ignored its context for " + d.String())
	}
	return rec, rec.Code < 500 || ctx.Err() == nil
}

// plainHeader reports whether name and value can arrive over HTTP/1.1 as given.
func plainHeader(name, value string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return !strings.ContainsAny(value, "\r\n\x00") && len(value) < 8<<10
}

func isJSON(b []byte) bool { return json.Valid(b) }

// FuzzIngest sends hostile deliveries to every kind of inbox. The answer is
// always one of the documented statuses, a delivery is stored as it came, and
// the credentials that must not be kept are not.
func FuzzIngest(f *testing.F) {
	e := newFuzzEnv(f)
	f.Add(uint8(0), uint8(0), "", "a=b", "X-Sig", "v", "X-Ts", "1800000000", []byte(`{"type":"x"}`))
	f.Add(uint8(1), uint8(1), "/a/b/../c", "", "X-Hub-Signature-256", "sha256=00", "Authorization", "Bearer x", []byte(""))
	f.Add(uint8(2), uint8(2), "//x", "%zz", "X-Inbox-Token", "tok-secret", "Cookie", "a=b", []byte("\x00\xff"))
	f.Add(uint8(3), uint8(3), "/%2e%2e/%00", "a;b", "Stripe-Signature", "t=1,v1=zz", "X-Id", "i", []byte(`{"id":"e","type":"t"}`))
	f.Add(uint8(4), uint8(4), "/"+strings.Repeat("a/", 100), strings.Repeat("&", 3000), "X-Slack-Signature", "v0=", "X-Slack-Request-Timestamp", "-1", []byte(`{"type":"url_verification","challenge":"c"}`))
	f.Add(uint8(5), uint8(5), "/x", "", "X-Sig", "AA==", "X-Ts", "01800000000", []byte(`{}`))
	f.Add(uint8(6), uint8(6), "/x", "", "webhook-id", "m.1", "webhook-signature", "v1,AAAA", []byte(`{}`))
	f.Fuzz(func(t *testing.T, which, method uint8, path, rawQuery, h1, v1, h2, v2 string, body []byte) {
		slugs := []string{"fz-none", "fz-github", "fz-token", "fz-stripe", "fz-slack", "fz-hmac", "fz-sw"}
		slug := slugs[int(which)%len(slugs)]
		if path != "" && !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		methods := []string{http.MethodPost, http.MethodPut, http.MethodGet, http.MethodDelete}
		u := &url.URL{Path: "/in/" + slug + path, RawQuery: rawQuery}
		r := &http.Request{Method: methods[int(method)%len(methods)], URL: u, RequestURI: u.RequestURI(), Header: http.Header{},
			Host: "example.test", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, RemoteAddr: "192.0.2.1:1234",
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
		sent := http.Header{}
		for _, kv := range [][2]string{{h1, v1}, {h2, v2}} {
			if plainHeader(kv[0], kv[1]) {
				r.Header.Add(kv[0], kv[1])
				sent.Add(kv[0], kv[1])
			}
		}
		rec, live := e.serve(r, 5*time.Second)
		if !live {
			return
		}
		switch rec.Code {
		case http.StatusOK, http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed,
			http.StatusBadRequest, http.StatusMovedPermanently, http.StatusPermanentRedirect, http.StatusTemporaryRedirect:
		default:
			t.Fatalf("%s %s: status %d: %.200s", r.Method, u, rec.Code, rec.Body)
		}
		if rec.Code != http.StatusOK {
			return
		}
		// Whatever was accepted was accepted by the inbox's own rules.
		switch slug {
		case "fz-token":
			if sent.Get("X-Inbox-Token") != fuzzSecrets[slug] {
				t.Fatalf("token inbox accepted %q", sent.Get("X-Inbox-Token"))
			}
		case "fz-github":
			mac := hmac.New(sha256.New, []byte(fuzzSecrets[slug]))
			mac.Write(body)
			if !strings.EqualFold(sent.Get("X-Hub-Signature-256"), "sha256="+hex.EncodeToString(mac.Sum(nil))) {
				t.Fatalf("github inbox accepted %q", sent.Get("X-Hub-Signature-256"))
			}
		}
		if rec.Header().Get("Content-Type") == "application/json" && !isJSON(rec.Body.Bytes()) {
			t.Fatalf("invalid JSON answer: %q", rec.Body)
		}
		// Look at what was stored (the newest message of the inbox).
		pg, err := e.store.Query(context.Background(), e.ins[slug].ID, inbox.Query{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Header().Get("Content-Type") != "application/json" || len(pg.Messages) == 0 {
			return // a handshake answer, which is not stored
		}
		m := pg.Messages[0]
		if m.Verified && m.VerifyError != "" {
			t.Fatal("a verified message carries a verify error")
		}
		if !bytes.Equal(m.Body, body) {
			t.Fatalf("stored body %q, sent %q", m.Body, body)
		}
		for _, name := range inbox.AlwaysRedactedHeaders {
			if _, ok := m.Headers[http.CanonicalHeaderKey(name)]; ok {
				t.Fatalf("stored the %s header", name)
			}
		}
		if slug == "fz-token" {
			if _, ok := m.Headers["X-Inbox-Token"]; ok {
				t.Fatal("stored the shared secret")
			}
		}
	})
}

// FuzzReadAPI sends hostile query strings and path parameters to the read
// endpoints, with the admin token.
func FuzzReadAPI(f *testing.F) {
	e := newFuzzEnv(f)
	f.Add(uint8(0), "", "after=0&limit=2")
	f.Add(uint8(0), "", "before=3&limit=1&fields=summary&type=a.created&verified=true")
	f.Add(uint8(0), "", "since=2026-01-01T00:00:00Z&until=2030-01-01T00:00:00Z&delivery_id=x&epoch=abc")
	f.Add(uint8(0), "", "after=99999999999999999999&limit=-1")
	f.Add(uint8(0), "", "after=1&after=2&limit=1e3&type=&type=")
	f.Add(uint8(1), "1", "epoch=%zz")
	f.Add(uint8(2), "2", "")
	f.Add(uint8(2), "0", "epoch=x")
	f.Add(uint8(3), "", "after=0&type=a&verified=maybe")
	f.Add(uint8(3), "", "%")
	f.Add(uint8(4), "", "")
	f.Fuzz(func(t *testing.T, which uint8, seq, rawQuery string) {
		var path string
		switch which % 5 {
		case 0:
			path = "/v1/inboxes/fz-none/messages"
		case 1:
			path = "/v1/inboxes/fz-none/messages/" + url.PathEscape(seq)
		case 2:
			path = "/v1/inboxes/fz-none/messages/" + url.PathEscape(seq) + "/raw"
		case 3:
			path = "/v1/inboxes/fz-none/stream"
		default:
			path = "/v1/inboxes/" + url.PathEscape(seq)
		}
		r := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: path, RawQuery: rawQuery}, Header: http.Header{"Authorization": {"Bearer " + fuzzAdminToken}},
			Host: "example.test", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, RemoteAddr: "192.0.2.1:1234", Body: http.NoBody}
		rec, live := e.serve(r, 100*time.Millisecond)
		if !live {
			return
		}
		if rec.Code >= 500 {
			t.Fatalf("GET %s?%s: status %d: %.200s", path, rawQuery, rec.Code, rec.Body)
		}
		if rec.Body.Len() > 1<<20 {
			t.Fatalf("a %d byte answer", rec.Body.Len())
		}
		ct := rec.Header().Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") && !isJSON(rec.Body.Bytes()) {
			t.Fatalf("invalid JSON answer: %.200q", rec.Body)
		}
		if rec.Code == http.StatusOK && which%5 == 0 {
			var page struct {
				Messages []json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Messages) > inbox.MaxQueryLimit {
				t.Fatalf("bad page (%d messages): %v", len(page.Messages), err)
			}
		}
	})
}

// FuzzStreamHeaders sends hostile Last-Event-ID headers and queries to the
// stream. A stream that opens must be well-formed SSE.
func FuzzStreamHeaders(f *testing.F) {
	e := newFuzzEnv(f)
	f.Add("2", "")
	f.Add("", "after=1")
	f.Add("-1", "after=x")
	f.Add("99999999999999999999", "epoch=abc&type=a.created&verified=false")
	f.Add("1\x00", "")
	f.Fuzz(func(t *testing.T, lastID, rawQuery string) {
		r := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/v1/inboxes/fz-none/stream", RawQuery: rawQuery}, Header: http.Header{"Authorization": {"Bearer " + fuzzAdminToken}},
			Host: "example.test", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, RemoteAddr: "192.0.2.1:1234", Body: http.NoBody}
		if plainHeader("Last-Event-ID", lastID) {
			r.Header.Set("Last-Event-ID", lastID)
		}
		rec, live := e.serve(r, 50*time.Millisecond)
		if !live {
			return
		}
		if rec.Code >= 500 {
			t.Fatalf("status %d: %.200s", rec.Code, rec.Body)
		}
		if rec.Code != http.StatusOK {
			if !isJSON(rec.Body.Bytes()) {
				t.Fatalf("invalid JSON error: %.200q", rec.Body)
			}
			return
		}
		for _, frame := range strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n") {
			for _, line := range strings.Split(frame, "\n") {
				switch {
				case strings.HasPrefix(line, "id: "), strings.HasPrefix(line, "event: "), strings.HasPrefix(line, "retry: "), strings.HasPrefix(line, ":"):
				case strings.HasPrefix(line, "data: "):
					if !isJSON([]byte(strings.TrimPrefix(line, "data: "))) {
						t.Fatalf("data line is not JSON: %.200q", line)
					}
				case line == "":
				default:
					t.Fatalf("unexpected line in the stream: %.200q", line)
				}
			}
		}
	})
}

// FuzzMCP sends arbitrary bytes, and mutated JSON-RPC calls, to /mcp.
func FuzzMCP(f *testing.F) {
	e := newFuzzEnv(f)
	add := func(s string) { f.Add([]byte(s), "application/json", "Bearer "+fuzzAdminToken) }
	add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	add(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	add(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	add(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_inboxes","arguments":{}}}`)
	add(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"query_messages","arguments":{"inbox":"fz-none","after":0,"limit":2,"bodies":true,"type":["a.created"],"since":"2026-01-01T00:00:00Z"}}}`)
	add(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"query_messages","arguments":{"inbox":"fz-none","after":-1,"before":9223372036854775807,"limit":0,"epoch":"%"}}}`)
	add(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_message","arguments":{"inbox":"fz-none","seq":1}}}`)
	add(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait_for_messages","arguments":{"inbox":"fz-none","after":99,"timeout_seconds":60}}}`)
	add(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"wait_for_messages","arguments":{"inbox":"fz-none","after":0,"timeout_seconds":-1,"limit":1e400}}}`)
	add(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`)
	add(`{"jsonrpc":"1.0","id":null,"method":""}`)
	add(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_message","arguments":{"inbox":"` + strings.Repeat("a", 1000) + `","seq":1e999}}}`)
	add(`{"jsonrpc":"2.0","id":` + strings.Repeat("[", 3000) + `}`)
	f.Add([]byte(`{`), "text/plain", "")
	f.Add([]byte(``), "application/json; charset=utf-16", "bearer x")
	f.Fuzz(func(t *testing.T, body []byte, ctype, auth string) {
		r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/mcp"}, Header: http.Header{"Accept": {"application/json, text/event-stream"}},
			Host: "example.test", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, RemoteAddr: "192.0.2.1:1234",
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
		if plainHeader("Content-Type", ctype) {
			r.Header.Set("Content-Type", ctype)
		}
		if plainHeader("Authorization", auth) {
			r.Header.Set("Authorization", auth)
		}
		rec, live := e.serve(r, 200*time.Millisecond)
		if !live {
			return
		}
		if rec.Code >= 500 {
			t.Fatalf("status %d: %.300s (for %.300q)", rec.Code, rec.Body, body)
		}
		if rec.Body.Len() > 8<<20 {
			t.Fatalf("a %d byte answer", rec.Body.Len())
		}
		if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") && !isJSON(rec.Body.Bytes()) {
			t.Fatalf("invalid JSON answer: %.300q", rec.Body)
		}
	})
}

// FuzzClientIP: the source address is the peer unless the peer is a trusted
// proxy, and then only an address from the header that is not itself a trusted
// proxy; anything else the header says is never believed.
func FuzzClientIP(f *testing.F) {
	f.Add("203.0.113.9:4000", "198.51.100.1, 10.0.0.2", "10.0.0.0/8", "10.0.0.1")
	f.Add("10.0.0.1:4000", "evil, 198.51.100.1", "10.0.0.0/8", "")
	f.Add("[::ffff:10.0.0.1]:4000", "::ffff:198.51.100.1%eth0", "10.0.0.0/8,::/0", "x")
	f.Add("garbage", "1.1.1.1", "0.0.0.0/0", "")
	f.Add("10.0.0.1", ",,,", "10.0.0.0/8", ",")
	f.Fuzz(func(t *testing.T, remote, xff1, prefixes, xff2 string) {
		var trusted []netip.Prefix
		for p := range strings.SplitSeq(prefixes, ",") {
			if pf, err := netip.ParsePrefix(strings.TrimSpace(p)); err == nil {
				trusted = append(trusted, pf)
			}
		}
		r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
		for _, v := range []string{xff1, xff2} {
			if plainHeader("X-Forwarded-For", v) {
				r.Header.Add("X-Forwarded-For", v)
			}
		}
		got := clientIP(r, trusted)
		if got == "" {
			return
		}
		addr, err := netip.ParseAddr(got)
		if err != nil {
			t.Fatalf("clientIP = %q, not an address", got)
		}
		peer, ok := peerAddr(remote)
		if !ok {
			t.Fatalf("an address %q for the unparsable peer %q", got, remote)
		}
		if addr == peer {
			return
		}
		if !containsAddr(trusted, peer) {
			t.Fatalf("peer %v is not a trusted proxy, but the address is %v", peer, addr)
		}
		if containsAddr(trusted, addr) {
			t.Fatalf("returned %v, which is itself a trusted proxy", addr)
		}
	})
}

// FuzzAdminJSON sends hostile JSON bodies to the admin API's writers.
func FuzzAdminJSON(f *testing.F) {
	e := newFuzzEnv(f)
	f.Add(uint8(0), []byte(`{"slug":"abc","name":"A","verifier":"none"}`))
	f.Add(uint8(0), []byte(`{"slug":"abc","name":"A","verifier":"hmac","verifier_config":{"header":"X-Sig"},"secret":"s","max_body_bytes":1e3,"retention_days":-1}`))
	f.Add(uint8(1), []byte(`{"name":"B","disabled":true,"on_verify_fail":"store","version":1}`))
	f.Add(uint8(2), []byte(`{"secret":"new"}`))
	f.Add(uint8(3), []byte(`{"name":"t","inbox_slugs":["fz-none"]}`))
	f.Add(uint8(0), []byte(`{"slug":"abc","name":"A","verifier":"none"}{"x":1}`))
	f.Add(uint8(0), []byte(strings.Repeat("[", 5000)))
	f.Add(uint8(1), []byte(`{"verifier_config":null}`))
	f.Add(uint8(0), []byte("\xef\xbb\xbf{}"))
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		paths := []struct{ method, path string }{
			{http.MethodPost, "/v1/admin/inboxes"},
			{http.MethodPatch, "/v1/admin/inboxes/fz-github"},
			{http.MethodPut, "/v1/admin/inboxes/fz-github/secret"},
			{http.MethodPost, "/v1/admin/tokens"},
		}
		p := paths[int(which)%len(paths)]
		r := &http.Request{Method: p.method, URL: &url.URL{Path: p.path}, Header: http.Header{"Authorization": {"Bearer " + fuzzAdminToken}, "Content-Type": {"application/json"}},
			Host: "example.test", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, RemoteAddr: "192.0.2.1:1234",
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
		rec, live := e.serve(r, 2*time.Second)
		if !live {
			return
		}
		if rec.Code >= 500 {
			t.Fatalf("%s %s: status %d: %.300s (for %.300q)", p.method, p.path, rec.Code, rec.Body, body)
		}
		if !isJSON(rec.Body.Bytes()) && rec.Body.Len() > 0 {
			t.Fatalf("invalid JSON answer: %.300q", rec.Body)
		}
		// A secret must never be echoed back in an error.
		if rec.Code >= 400 && bytes.Contains(rec.Body.Bytes(), []byte(fuzzSecrets["fz-github"])) {
			t.Fatal("an error echoes the stored secret")
		}
	})
}
