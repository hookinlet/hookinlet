// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/benchseed"
	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

const testAdminToken = "admin-token-for-agent-tests"

// agentEnv is an env with the read API turned on, and a token for each of two
// inboxes.
type agentEnv struct {
	*env
	a, b           *inbox.Inbox
	tokenA, tokenB string
}

func withAuth(o *Options) { o.Authenticator = token.NewAuthenticator(o.Store, testAdminToken) }

func newAgentEnv(t *testing.T) *agentEnv {
	t.Helper()
	e := newEnv(t, withAuth)
	ae := &agentEnv{env: e}
	mk := func(slug string) (*inbox.Inbox, string) {
		tok, hash, err := token.Generate()
		if err != nil {
			t.Fatal(err)
		}
		in := &inbox.Inbox{Slug: slug, IngestKey: slug, Name: "Inbox " + slug, Verifier: "trusted"}
		if err := e.store.CreateInbox(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if err := e.store.CreateToken(t.Context(), &inbox.Token{Name: slug, Hash: hash, InboxIDs: []inbox.ID{in.ID}}); err != nil {
			t.Fatal(err)
		}
		return in, tok
	}
	ae.a, ae.tokenA = mk("agent-a")
	ae.b, ae.tokenB = mk("agent-b")
	return ae
}

func (e *env) get(path, tok string) *httptest.ResponseRecorder {
	e.t.Helper()
	h := http.Header{}
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return e.do(req{method: http.MethodGet, path: path, headers: h})
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %q (%v)", rec.Body.String(), err)
	}
	return v
}

type apiMessage struct {
	Seq         int64               `json:"seq"`
	Inbox       string              `json:"inbox"`
	ReceivedAt  string              `json:"received_at"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Query       string              `json:"query"`
	Headers     map[string][]string `json:"headers"`
	ContentType string              `json:"content_type"`
	Size        int64               `json:"size"`
	Verified    bool                `json:"verified"`
	Verifier    string              `json:"verifier"`
	VerifyError *string             `json:"verify_error"`
	DeliveryID  *string             `json:"delivery_id"`
	EventType   *string             `json:"event_type"`
	SourceIP    *string             `json:"source_ip"`
	Body        json.RawMessage     `json:"body"`
	BodyText    *string             `json:"body_text"`
	BodyBase64  *string             `json:"body_base64"`
}

type apiPage struct {
	Messages  []apiMessage `json:"messages"`
	Next      *string      `json:"next"`
	LatestSeq int64        `json:"latest_seq"`
}

// walk follows next links from path with the given token until the walk ends,
// and returns every message in the order received.
func (e *env) walk(path, tok string) []apiMessage {
	e.t.Helper()
	var all []apiMessage
	for i := 0; i < 5000; i++ {
		rec := e.get(path, tok)
		if rec.Code != http.StatusOK {
			e.t.Fatalf("GET %s: status %d, %s", path, rec.Code, rec.Body.String())
		}
		page := decode[apiPage](e.t, rec)
		all = append(all, page.Messages...)
		if page.Next == nil || len(page.Messages) == 0 {
			return all
		}
		path = *page.Next
	}
	e.t.Fatal("paging does not terminate")
	return nil
}

func seqsOfAPI(ms []apiMessage) []int64 {
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Seq)
	}
	return out
}

// ---- authentication and authorization ----

func TestReadAPIRequiresAuthentication(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", `{"n":1}`)
	paths := []string{
		"/v1/inboxes/agent-a", "/v1/inboxes/agent-a/messages",
		"/v1/inboxes/agent-a/messages/1", "/v1/inboxes/agent-a/messages/1/raw",
		// An unauthenticated caller gets 401 even for inboxes that do not exist,
		// so the status never reveals which inboxes there are.
		"/v1/inboxes/no-such-inbox", "/v1/inboxes/no-such-inbox/messages", "/v1/inboxes/no-such-inbox/messages/1/raw",
	}
	for _, path := range paths {
		for name, tok := range map[string]string{"no token": "", "wrong token": "hki_not-a-real-token", "garbage": "x y z"} { //nolint:gosec // made-up values
			rec := e.get(path, tok)
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthenticated" {
				t.Errorf("%s with %s: status %d, %s", path, name, rec.Code, rec.Body.String())
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s: no WWW-Authenticate header", path)
			}
		}
	}
	// A token in the query string is not a credential.
	if rec := e.get("/v1/inboxes/agent-a/messages?access_token="+e.tokenA, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a token in the URL authenticated: status %d", rec.Code)
	}
}

func TestATokenReadsOnlyItsOwnInbox(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", `{"who":"a"}`)
	e.post("/in/agent-b", `{"who":"b"}`)

	endpoints := func(slug string) []string {
		return []string{"/v1/inboxes/" + slug, "/v1/inboxes/" + slug + "/messages", "/v1/inboxes/" + slug + "/messages/1", "/v1/inboxes/" + slug + "/messages/1/raw"}
	}
	for i, path := range endpoints("agent-a") {
		if rec := e.get(path, e.tokenA); rec.Code != http.StatusOK {
			t.Errorf("A reading A (%s): status %d, %s", path, rec.Code, rec.Body.String())
		}
		if rec := e.get(path, testAdminToken); rec.Code != http.StatusOK {
			t.Errorf("admin reading A (%s): status %d", path, rec.Code)
		}

		// A's token against B, and against an inbox that does not exist, must be
		// the same response, byte for byte.
		other := endpoints("agent-b")[i]
		ghost := endpoints("never-created")[i]
		cross, missing := e.get(other, e.tokenA), e.get(ghost, e.tokenA)
		if cross.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
			t.Errorf("%s: A on B = %d, A on a ghost = %d; want 404 for both", path, cross.Code, missing.Code)
			continue
		}
		if cross.Body.String() != missing.Body.String() {
			t.Errorf("A on B and A on a ghost differ:\n%q\n%q", cross.Body.String(), missing.Body.String())
		}
		if strings.Contains(cross.Body.String(), "agent-b") {
			t.Errorf("the 404 names the other inbox: %s", cross.Body.String())
		}
		if got := cross.Header().Get("Content-Type"); got != missing.Header().Get("Content-Type") {
			t.Errorf("content types differ: %q vs %q", got, missing.Header().Get("Content-Type"))
		}
		// And B's token reads B but not A.
		if rec := e.get(endpoints("agent-b")[i], e.tokenB); rec.Code != http.StatusOK {
			t.Errorf("B reading B: status %d", rec.Code)
		}
		if rec := e.get(path, e.tokenB); rec.Code != http.StatusNotFound {
			t.Errorf("B reading A: status %d", rec.Code)
		}
	}
	// B's message bodies never appear in anything A could fetch.
	for _, path := range endpoints("agent-a") {
		if strings.Contains(e.get(path, e.tokenA).Body.String(), `"who":"b"`) {
			t.Errorf("%s leaked B's data to A", path)
		}
	}
}

func TestOnlyGetAndHeadAreAllowed(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "x")
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		for _, path := range []string{"/v1/inboxes/agent-a", "/v1/inboxes/agent-a/messages", "/v1/inboxes/agent-a/messages/1/raw"} {
			rec := e.do(req{method: method, path: path, headers: http.Header{"Authorization": {"Bearer " + e.tokenA}}})
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" || errorCode(t, rec) != "method_not_allowed" {
				t.Errorf("%s %s: status %d, allow %q", method, path, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
	// A write attempt changed nothing.
	if n := e.count(e.a); n != 1 {
		t.Errorf("%d messages after rejected writes", n)
	}
}

func TestAnAuthenticatorFailureIsAServerError(t *testing.T) {
	e := newAgentEnv(t)
	_ = e.store.Close()
	for name, tok := range map[string]string{"agent token (needs a lookup)": e.tokenA, "admin token (then needs the inbox)": testAdminToken} {
		rec := e.get("/v1/inboxes/agent-a", tok)
		if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != "internal" {
			t.Errorf("%s: status %d, %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestReadAPIIsOffWithoutAnAuthenticator(t *testing.T) {
	e := newEnv(t) // no Authenticator
	e.newInbox("quiet-box", "trusted", "")
	for _, path := range []string{"/v1/inboxes/quiet-box", "/v1/inboxes/quiet-box/messages"} {
		if rec := e.get(path, "anything"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 (not served)", path, rec.Code)
		}
	}
}

func TestADisabledInboxCanStillBeRead(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", `{"kept":true}`)
	off := *e.a
	off.Disabled = true
	if err := e.store.UpdateInbox(t.Context(), &off); err != nil {
		t.Fatal(err)
	}
	if rec := e.post("/in/agent-a", "more"); rec.Code != http.StatusNotFound {
		t.Fatalf("ingest to a disabled inbox: status %d", rec.Code)
	}
	if rec := e.get("/v1/inboxes/agent-a/messages", e.tokenA); rec.Code != http.StatusOK || len(decode[apiPage](t, rec).Messages) != 1 {
		t.Errorf("a disabled inbox should be drainable: status %d", rec.Code)
	}
}

// ---- the inbox, message, and page responses ----

func TestInboxInfo(t *testing.T) {
	e := newAgentEnv(t)
	for range 3 {
		e.post("/in/agent-a", "x")
	}
	rec := e.get("/v1/inboxes/agent-a", e.tokenA)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	info := decode[map[string]any](t, rec)
	want := map[string]any{"slug": "agent-a", "name": "Inbox agent-a", "verifier": "trusted", "message_count": 3.0, "oldest_seq": 1.0, "latest_seq": 3.0, "epoch": e.a.Epoch}
	if e.a.Epoch == "" {
		t.Error("the inbox has no epoch")
	}
	if len(info) != len(want) {
		t.Errorf("info has fields %v, want exactly %v", info, want)
	}
	for k, v := range want {
		if info[k] != v {
			t.Errorf("%s = %v, want %v", k, info[k], v)
		}
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Request-Id") == "" {
		t.Errorf("headers = %v", rec.Header())
	}
	// An empty inbox.
	if got := decode[map[string]any](t, e.get("/v1/inboxes/agent-b", e.tokenB)); got["message_count"] != 0.0 || got["latest_seq"] != 0.0 || got["oldest_seq"] != 0.0 {
		t.Errorf("empty inbox info = %v", got)
	}
}

func TestMessageResponse(t *testing.T) {
	e := newAgentEnv(t)
	e.do(req{method: http.MethodPut, path: "/in/agent-a/orders/9?x=1", body: `{"id":42,"ok":true}`, remote: "198.51.100.4:1",
		headers: http.Header{"Content-Type": {"application/json"}, "X-Custom": {"v"}, "Authorization": {"Bearer must-not-appear"}}})

	rec := e.get("/v1/inboxes/agent-a/messages/1", e.tokenA)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, %s", rec.Code, rec.Body.String())
	}
	m := decode[apiMessage](t, rec)
	if m.Seq != 1 || m.Inbox != "agent-a" || m.Method != "PUT" || m.Path != "orders/9" || m.Query != "x=1" || m.ContentType != "application/json" ||
		m.Verified != true || m.Verifier != "trusted" || m.Size != int64(len(`{"id":42,"ok":true}`)) || m.SourceIP == nil || *m.SourceIP != "198.51.100.4" {
		t.Errorf("message = %+v", m)
	}
	if string(m.Body) != `{"id":42,"ok":true}` || m.BodyText != nil || m.BodyBase64 != nil {
		t.Errorf("body fields: body=%s text=%v b64=%v", m.Body, m.BodyText, m.BodyBase64)
	}
	if m.Headers["X-Custom"][0] != "v" {
		t.Errorf("headers = %v", m.Headers)
	}
	if strings.Contains(rec.Body.String(), "must-not-appear") {
		t.Error("a redacted header appears in the response")
	}
	if !strings.HasSuffix(m.ReceivedAt, "Z") || len(m.ReceivedAt) != len("2026-03-01T12:00:00.000000Z") {
		t.Errorf("received_at = %q", m.ReceivedAt)
	}
}

func TestBodyRepresentationsOverHTTP(t *testing.T) {
	e := newAgentEnv(t)
	bodies := []struct {
		body []byte
		kind string
	}{
		{[]byte(`{"a":[1,2,3]}`), "body"},
		{[]byte(`plain text`), "body_text"},
		{[]byte(``), "body_text"},
		{[]byte{0xff, 0xfe, 0x00, 0x80}, "body_base64"},
	}
	for _, b := range bodies {
		e.do(req{path: "/in/agent-a", body: string(b.body)})
	}
	page := decode[apiPage](t, e.get("/v1/inboxes/agent-a/messages?after=0", e.tokenA))
	for i, b := range bodies {
		m := page.Messages[i]
		present := map[string]bool{"body": m.Body != nil, "body_text": m.BodyText != nil, "body_base64": m.BodyBase64 != nil}
		for kind, has := range present {
			if has != (kind == b.kind) {
				t.Errorf("message %d (%q): %s present = %v", i+1, b.body, kind, has)
			}
		}
		if m.BodyBase64 != nil {
			got, err := base64.StdEncoding.DecodeString(*m.BodyBase64)
			if err != nil || !bytes.Equal(got, b.body) {
				t.Errorf("message %d: base64 does not round trip", i+1)
			}
		}
	}
}

func TestSummaryFields(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", `{"big":"`+strings.Repeat("x", 1000)+`"}`)
	rec := e.get("/v1/inboxes/agent-a/messages?after=0&fields=summary", e.tokenA)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	raw := decode[map[string]any](t, rec)["messages"].([]any)[0].(map[string]any)
	for _, k := range []string{"body", "body_text", "body_base64"} {
		if _, ok := raw[k]; ok {
			t.Errorf("a summary has %s", k)
		}
	}
	if raw["size"].(float64) < 1000 {
		t.Errorf("size = %v", raw["size"])
	}
	if next := decode[apiPage](t, rec).Next; next == nil || !strings.Contains(*next, "fields=summary") {
		t.Errorf("next = %v; the summary flag must carry over", next)
	}
}

// ---- parameters ----

func TestParametersAreStrict(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "x")
	for _, q := range []string{"limt=5", "after=1&before=2", "after=abc", "limit=0", "limit=-3", "type=", "verified=maybe", "since=yesterday", "fields=all", "after=1&after=2"} {
		rec := e.get("/v1/inboxes/agent-a/messages?"+q, e.tokenA)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_parameter" {
			t.Errorf("?%s: status %d, %s", q, rec.Code, rec.Body.String())
		}
	}
	// The single-message endpoints take no parameters at all.
	for _, path := range []string{"/v1/inboxes/agent-a", "/v1/inboxes/agent-a/messages/1", "/v1/inboxes/agent-a/messages/1/raw"} {
		if rec := e.get(path+"?x=1", e.tokenA); rec.Code != http.StatusBadRequest {
			t.Errorf("%s?x=1: status %d", path, rec.Code)
		}
	}
	if rec := e.get("/v1/inboxes/agent-a/messages?limit=100000", e.tokenA); rec.Code != http.StatusOK {
		t.Errorf("an oversized limit is clamped, not an error: status %d", rec.Code)
	}
}

func TestMessageNumbersThatCannotExistAre404(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "x")
	for _, seq := range []string{"0", "-1", "01", "+1", "1.0", "abc", "1e3", "99999999999999999999", "2", "%20", "１"} {
		for _, suffix := range []string{"", "/raw"} {
			rec := e.get("/v1/inboxes/agent-a/messages/"+seq+suffix, e.tokenA)
			if rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found" {
				t.Errorf("seq %q%s: status %d, %s", seq, suffix, rec.Code, rec.Body.String())
			}
		}
	}
	if rec := e.get("/v1/inboxes/agent-a/messages/1", e.tokenA); rec.Code != http.StatusOK {
		t.Errorf("seq 1: status %d", rec.Code)
	}
}

// ---- /raw ----

func TestRawReturnsTheExactBytesAndContentType(t *testing.T) {
	e := newAgentEnv(t)
	every := make([]byte, 256)
	for i := range every {
		every[i] = byte(i)
	}
	cases := []struct {
		name, ctype string
		body        []byte
	}{
		{"binary with every byte", "application/octet-stream", every},
		{"invalid utf-8 with a charset", "text/plain; charset=iso-8859-1", []byte{0xe9, 0xe8, 0xff, 0x00}},
		{"json with odd whitespace", "application/json", []byte(" {\"a\" :\t1 ,\n\"a\":2}  ")},
		{"form data", "application/x-www-form-urlencoded", []byte("a=1&b=%20%ff")},
		{"crlf text", "text/plain", []byte("a\r\nb\n\rc")},
		{"an unusual type", "application/vnd.acme+x-weird; version=\"2\"; boundary=a b", []byte("zzz")},
		{"html", "text/html; charset=utf-8", []byte("<html><script>alert(document.cookie)</script></html>")},
		{"svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)},
		{"large", "application/octet-stream", bytes.Repeat([]byte{0xab, 0xcd}, 400_000)},
	}
	for i, c := range cases {
		hdr := http.Header{"Content-Type": {c.ctype}}
		in := e.a
		if c.name == "large" {
			in = e.newInbox("big-raw", "trusted", "", func(in *inbox.Inbox) { in.MaxBodyBytes = inbox.MaxBodyBytesCeiling })
		}
		if rec := e.do(req{path: "/in/" + in.Slug, body: string(c.body), headers: hdr}); rec.Code != http.StatusOK {
			t.Fatalf("%s: ingest status %d", c.name, rec.Code)
		}
		path := fmt.Sprintf("/v1/inboxes/%s/messages/%d/raw", in.Slug, map[bool]int{true: 1, false: i + 1}[c.name == "large"])
		rec := e.get(path, testAdminToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.name, rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), c.body) {
			t.Errorf("%s: the body differs (%d bytes back, %d sent)", c.name, rec.Body.Len(), len(c.body))
		}
		if got := rec.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type = %q, want %q", c.name, got, c.ctype)
		}
		if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(c.body)) {
			t.Errorf("%s: Content-Length = %q", c.name, got)
		}
		// Whatever the sender claimed, it is served as something to download,
		// never as a page that could run script from this origin.
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Disposition") != "attachment" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s: unsafe headers %v", c.name, h)
		}
	}
}

func TestRawWithoutAContentType(t *testing.T) {
	e := newAgentEnv(t)
	e.do(req{path: "/in/agent-a", body: "no type here"})
	rec := e.get("/v1/inboxes/agent-a/messages/1/raw", e.tokenA)
	if rec.Code != http.StatusOK || rec.Body.String() != "no type here" {
		t.Fatalf("status %d, %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q; a message with none must not be sniffed", got)
	}
	// An empty body is a real, empty response.
	e.do(req{path: "/in/agent-a", body: "", headers: http.Header{"Content-Type": {"text/plain"}}})
	if rec := e.get("/v1/inboxes/agent-a/messages/2/raw", e.tokenA); rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
		t.Errorf("empty body: status %d, len %d, length header %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
}

func TestHeadReturnsHeadersWithoutABody(t *testing.T) {
	e := newAgentEnv(t)
	e.do(req{path: "/in/agent-a", body: "twelve bytes", headers: http.Header{"Content-Type": {"text/plain"}}})
	for _, path := range []string{"/v1/inboxes/agent-a", "/v1/inboxes/agent-a/messages", "/v1/inboxes/agent-a/messages/1", "/v1/inboxes/agent-a/messages/1/raw"} {
		rec := e.do(req{method: http.MethodHead, path: path, headers: http.Header{"Authorization": {"Bearer " + e.tokenA}}})
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" || rec.Header().Get("Content-Length") == "0" {
			t.Errorf("HEAD %s: status %d, body %d bytes, Content-Length %q", path, rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
		}
	}
}

// ---- secrets ----

func TestNoResponseContainsASecret(t *testing.T) {
	e := newAgentEnv(t)
	box, _ := secretbox.New(bytes.Repeat([]byte{5}, secretbox.KeySize))
	sealed, _ := box.Seal([]byte("the-verifier-secret-value"), []byte("has-secrets"))
	tok, hash, _ := token.Generate()
	in := &inbox.Inbox{Slug: "has-secrets", IngestKey: "has-secrets", Name: "n", Verifier: "token", VerifierSecret: sealed}
	if err := e.store.CreateInbox(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateToken(t.Context(), &inbox.Token{Name: "t", Hash: hash, InboxIDs: []inbox.ID{in.ID}}); err != nil {
		t.Fatal(err)
	}
	e.post("/in/has-secrets", "payload", "X-Inbox-Token", "the-verifier-secret-value")

	forbidden := [][]byte{
		[]byte("the-verifier-secret-value"), sealed, hash, []byte(hex.EncodeToString(hash)), []byte(base64.StdEncoding.EncodeToString(hash)),
		[]byte(base64.StdEncoding.EncodeToString(sealed)), []byte(hex.EncodeToString(sealed)), []byte(tok), []byte(testAdminToken),
		[]byte("verifier_secret"), []byte("agent_token_hash"), []byte("token_hash"),
	}
	for _, path := range []string{"/v1/inboxes/has-secrets", "/v1/inboxes/has-secrets/messages", "/v1/inboxes/has-secrets/messages/1", "/v1/inboxes/has-secrets/messages/1/raw"} {
		for _, who := range []string{tok, testAdminToken} {
			rec := e.get(path, who)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d", path, rec.Code)
			}
			wire := rec.Body.Bytes()
			for k, v := range rec.Header() {
				wire = append(wire, []byte(k+": "+strings.Join(v, ","))...)
			}
			for _, f := range forbidden {
				if bytes.Contains(wire, f) {
					t.Errorf("%s leaks %.24q", path, f)
				}
			}
		}
	}
}

func TestTokensAreNeverLogged(t *testing.T) {
	e := newAgentEnv(t)
	e.post("/in/agent-a", "BODY-MARKER")
	e.get("/v1/inboxes/agent-a/messages", e.tokenA)
	e.get("/v1/inboxes/agent-b/messages", e.tokenA)
	e.get("/v1/inboxes/agent-a/messages", "hki_a-wrong-token-MARKER")
	e.get("/v1/inboxes/agent-a/messages/1/raw", testAdminToken)
	logs := e.logs.String()
	for _, secret := range []string{e.tokenA, e.tokenB, testAdminToken, "a-wrong-token-MARKER", "BODY-MARKER"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain %q", secret)
		}
	}
}

// ---- walking large inboxes through the whole stack ----

// seededServer builds a server over an inbox of n seeded messages (see
// benchseed for exactly what message i looks like) and returns a helper that
// reads it as the admin.
func seededServer(t *testing.T, n int) (*env, *inbox.Inbox) {
	t.Helper()
	dir := t.TempDir()
	in, err := benchseed.Seed(t.Context(), dir, "seeded", n)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logs := &syncBuffer{}
	srv, err := New(Options{Addr: "127.0.0.1:0", Logger: slog.New(slog.NewJSONHandler(logs, nil)), Store: store,
		Authenticator: token.NewAuthenticator(store, testAdminToken)})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, store: store, server: srv, logs: logs}, in
}

func TestWalkingTenThousandMessagesSeesEachExactlyOnce(t *testing.T) {
	const n = 10_000
	e, _ := seededServer(t, n)

	asc := e.walk("/v1/inboxes/seeded/messages?after=0&limit=500", testAdminToken)
	if len(asc) != n {
		t.Fatalf("ascending walk saw %d messages, want %d", len(asc), n)
	}
	for i, m := range asc {
		if m.Seq != int64(i+1) {
			t.Fatalf("ascending position %d holds seq %d: out of order, skipped, or repeated", i, m.Seq)
		}
	}

	desc := e.walk("/v1/inboxes/seeded/messages?limit=500", testAdminToken)
	if len(desc) != n {
		t.Fatalf("backward walk saw %d messages, want %d", len(desc), n)
	}
	for i, m := range desc {
		if m.Seq != int64(n-i) {
			t.Fatalf("backward position %d holds seq %d", i, m.Seq)
		}
	}

	// Small pages and a start in the middle.
	mid := e.walk("/v1/inboxes/seeded/messages?before=5001&limit=37", testAdminToken)
	if len(mid) != 5000 || mid[0].Seq != 5000 || mid[len(mid)-1].Seq != 1 {
		t.Errorf("walk from the middle: %d messages, first %d, last %d", len(mid), mid[0].Seq, mid[len(mid)-1].Seq)
	}
}

func TestFiltersCombineCorrectly(t *testing.T) {
	const n = 3000
	e, _ := seededServer(t, n)
	// Message i has type "type-<i mod 20>", is verified unless i is a multiple
	// of 100, and arrived i seconds after benchseed.Start. The expectations are
	// computed from that rule alone, not from the server.
	expect := func(keep func(i int) bool) []int64 {
		var out []int64
		for i := 1; i <= n; i++ {
			if keep(i) {
				out = append(out, int64(i))
			}
		}
		return out
	}
	at := func(sec int) string {
		return benchseed.Start.Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
	}

	tests := []struct {
		name  string
		query string
		keep  func(i int) bool
	}{
		{"one type", "type=type-7", func(i int) bool { return i%20 == 7 }},
		{"two types", "type=type-3&type=type-11", func(i int) bool { return i%20 == 3 || i%20 == 11 }},
		{"unverified only", "verified=false", func(i int) bool { return i%100 == 0 }},
		{"verified only", "verified=true&type=type-0", func(i int) bool { return i%20 == 0 && i%100 != 0 }},
		{"a type that is also unverified", "type=type-0&verified=false", func(i int) bool { return i%100 == 0 }},
		{"since is inclusive", "since=" + at(1000), func(i int) bool { return i >= 1000 }},
		{"until is exclusive", "until=" + at(500), func(i int) bool { return i < 500 }},
		{"a time window", "since=" + at(1000) + "&until=" + at(2000), func(i int) bool { return i >= 1000 && i < 2000 }},
		{"window + type", "since=" + at(1000) + "&until=" + at(2000) + "&type=type-5", func(i int) bool { return i >= 1000 && i < 2000 && i%20 == 5 }},
		{"window + type + verified", "since=" + at(100) + "&until=" + at(2900) + "&type=type-0&verified=true", func(i int) bool { return i >= 100 && i < 2900 && i%20 == 0 && i%100 != 0 }},
		{"delivery id", "delivery_id=evt_1234", func(i int) bool { return i == 1234 }},
		{"delivery id of an unverified message", "delivery_id=evt_1200", func(i int) bool { return false }},
		{"delivery id and a contradicting type", "delivery_id=evt_1234&type=type-0", func(i int) bool { return false }},
		{"an unknown type", "type=nope", func(i int) bool { return false }},
	}
	for _, tt := range tests {
		for _, dir := range []struct{ name, cursor string }{{"ascending", "after=0&"}, {"newest first", ""}} {
			got := seqsOfAPI(e.walk("/v1/inboxes/seeded/messages?"+dir.cursor+tt.query+"&limit=100", testAdminToken))
			want := expect(tt.keep)
			if dir.name == "newest first" {
				slices.Reverse(want)
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s (%s): got %d messages, want %d\n got:  %.10v\n want: %.10v", tt.name, dir.name, len(got), len(want), got, want)
			}
		}
	}
}

// A reader following next while writers append through the real ingest endpoint
// must see every message exactly once and in order: the property the whole
// cursor design exists for, checked end to end.
func TestPollingWhileDeliveriesArriveSeesEachMessageOnce(t *testing.T) {
	e := newAgentEnv(t)
	const writers, perWriter = 6, 50
	const total = writers * perWriter

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				rec := e.post("/in/agent-a", fmt.Sprintf(`{"w":%d,"i":%d}`, w, i))
				if rec.Code != http.StatusOK {
					t.Errorf("ingest status %d", rec.Code)
					return
				}
			}
		})
	}

	seen := map[string]int{}
	var last int64
	next := "/v1/inboxes/agent-a/messages?after=0&limit=20"
	for ctx.Err() == nil && last < total {
		rec := e.get(next, e.tokenA)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, %s", rec.Code, rec.Body.String())
		}
		page := decode[apiPage](t, rec)
		for _, m := range page.Messages {
			if m.Seq != last+1 {
				t.Fatalf("saw seq %d after %d: a message was skipped or arrived late", m.Seq, last)
			}
			last = m.Seq
			seen[string(m.Body)]++
		}
		next = *page.Next
		if len(page.Messages) == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	wg.Wait()
	if last != total || len(seen) != total {
		t.Fatalf("saw %d messages (%d distinct), want %d", last, len(seen), total)
	}
	for body, n := range seen {
		if n != 1 {
			t.Errorf("%s seen %d times", body, n)
		}
	}
}

func TestUnverifiedMessagesAreMarkedAsSuch(t *testing.T) {
	e := newAgentEnv(t)
	in := e.newInbox("mixed-trust", "github", "s", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	body := `{"x":1}`
	e.post("/in/mixed-trust", body, "X-Hub-Signature-256", githubSig("wrong", body))
	e.post("/in/mixed-trust", body, "X-Hub-Signature-256", githubSig("s", body))
	_ = in

	all := e.walk("/v1/inboxes/mixed-trust/messages?after=0", testAdminToken)
	if len(all) != 2 || all[0].Verified || all[0].VerifyError == nil || *all[0].VerifyError != "signature mismatch" || !all[1].Verified || all[1].VerifyError != nil {
		t.Errorf("messages = %+v", all)
	}
	only := e.walk("/v1/inboxes/mixed-trust/messages?after=0&verified=true", testAdminToken)
	if len(only) != 1 || !only[0].Verified {
		t.Errorf("verified=true returned %+v", only)
	}
	// A stored forgery's claims are not trusted, so there are none to see.
	if all[0].DeliveryID != nil || all[0].EventType != nil {
		t.Errorf("an unverified message carries claims: %+v", all[0])
	}
	_ = url.Values{}
}
