// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/specvalidate"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// docs/openapi.json describes the HTTP API, and these tests hold it to the server
// from both sides. The document must list exactly the routes the server
// registers. Every operation is called, and every response the document lists
// for it must actually be seen (the unusual ones are forced with failing stores,
// hooks, and limits), with a body and headers that match. Nothing the server
// answers may be missing from the document. And the requests it describes are
// the requests the server accepts and rejects: invalid ones are generated from
// the schemas and must be refused.
//
// The schema checker below understands a fixed set of JSON Schema keywords and
// fails on any other, so a schema can never pass because a keyword was ignored.

// strict makes an object schema that lists properties and says nothing about
// additionalProperties closed. The published document leaves response objects
// open, so that a client written from it keeps working when the server adds a
// field; the server must not add one without the document, and this is what
// holds it to that.
type spec struct {
	root   map[string]any
	doc    *specvalidate.Document
	strict bool
}

func loadSpec(t *testing.T) *spec {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("docs/openapi.json is not valid JSON: %v", err)
	}
	// Load refuses a document that uses a keyword or format the checker would
	// ignore, so a schema cannot pass because part of it was not understood.
	doc, err := specvalidate.Load(b)
	if err != nil {
		t.Fatal(err)
	}
	return &spec{root: root, doc: doc, strict: true}
}

func get(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

type operation struct {
	id, method, path string
	obj              map[string]any
}

func (s *spec) operations() []operation {
	var out []operation
	for path, item := range s.root["paths"].(map[string]any) {
		for method, o := range item.(map[string]any) {
			om := o.(map[string]any)
			out = append(out, operation{om["operationId"].(string), strings.ToUpper(method), path, om})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (s *spec) op(id string) operation {
	for _, o := range s.operations() {
		if o.id == id {
			return o
		}
	}
	return operation{}
}

func (s *spec) deref(v any) any {
	for {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		r, ok := m["$ref"].(string)
		if !ok {
			return v
		}
		v = get(s.root, strings.Split(strings.TrimPrefix(r, "#/"), "/")...)
	}
}

// response returns the documented response for an operation and status
// ("default" for the catch-all), or nil.
func (s *spec) response(o operation, status string) map[string]any {
	r, _ := s.deref(get(o.obj, "responses", status)).(map[string]any)
	return r
}

// ---- the schema checker ----

// check validates v against schema with the checker the server itself uses
// (internal/specvalidate), with response objects closed, and returns every problem
// found, each prefixed with where it was.
func (s *spec) check(schema, v any, at string) []string {
	var out []string
	for _, viol := range s.doc.Check(schema, v, specvalidate.Options{ClosedObjects: s.strict}) {
		p := at
		if viol.Path != "" {
			p += "." + viol.Path
		}
		out = append(out, p+": "+viol.Reason)
	}
	return out
}

// ---- the document itself ----

// Every schema, parameter, and example in the document is checked by this test's
// own checker: it understands every keyword used, and each example is valid.
func TestOpenAPIIsInternallyConsistent(t *testing.T) {
	sp := loadSpec(t)
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			if ex, ok := x["example"]; ok {
				if _, isSchema := x["type"]; isSchema || x["$ref"] != nil {
					for _, e := range sp.check(x, ex, at+".example") {
						t.Error(e)
					}
				}
			}
			for k, c := range x {
				walk(c, at+"/"+k)
			}
		case []any:
			for i, c := range x {
				walk(c, fmt.Sprintf("%s[%d]", at, i))
			}
		}
	}
	walk(sp.root, "")

	// Every $ref resolves, and every schema uses only keywords the checker knows.
	var refs func(v any)
	refs = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if r, ok := x["$ref"].(string); ok && sp.deref(x) == nil {
				t.Errorf("%s does not resolve", r)
			}
			for _, c := range x {
				refs(c)
			}
		case []any:
			for _, c := range x {
				refs(c)
			}
		}
	}
	refs(sp.root)
	// A schema that uses a keyword the checker would ignore is refused by
	// loadSpec, so there is nothing more to check for here.

	// Operation ids are unique; every operation has a `default` response and a
	// request id on every response.
	seen := map[string]bool{}
	for _, o := range sp.operations() {
		if seen[o.id] {
			t.Errorf("operation id %q is used twice", o.id)
		}
		seen[o.id] = true
		if sp.response(o, "default") == nil {
			t.Errorf("%s has no default response", o.id)
		}
		for st := range o.obj["responses"].(map[string]any) {
			if get(sp.response(o, st), "headers", "X-Request-Id") == nil {
				t.Errorf("%s %s does not document X-Request-Id", o.id, st)
			}
		}
	}
}

// The document and the server list the same routes.
func TestOpenAPIListsExactlyTheRoutesTheServerRegisters(t *testing.T) {
	sp := loadSpec(t)
	documented := map[string]bool{}
	for p := range sp.root["paths"].(map[string]any) {
		documented[p] = true
	}
	// Routes are registered with literal patterns in the server's source, so read
	// them from there. {name...} is how net/http spells a path with slashes in it.
	re := regexp.MustCompile(`HandleFunc\("(?:[A-Z]+ )?(/[^"]*)"`)
	registered := map[string]bool{}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f) //nolint:gosec // a source file in this package's directory
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if p := strings.ReplaceAll(m[1], "...}", "}"); p != "/" { // "/" is the catch-all that answers 404
				registered[p] = true
			}
		}
	}
	var missing, extra []string
	for p := range registered {
		if !documented[p] {
			missing = append(missing, p)
		}
	}
	for p := range documented {
		if !registered[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("the server serves these but docs/openapi.json does not describe them: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("docs/openapi.json describes these but the server does not register them: %v", extra)
	}
	if len(registered) < 10 {
		t.Errorf("found only %d routes in the source; the pattern in this test no longer matches how they are registered", len(registered))
	}
}

// Operations that need no credential say so; the admin API says it needs the
// admin token and nothing else; the rest accept an agent token or the admin token.
func TestOpenAPISecurityMatchesTheServer(t *testing.T) {
	sp := loadSpec(t)
	open := map[string]bool{"healthz": true, "readyz": true, "ingest": true, "ingestWithPath": true}
	for _, o := range sp.operations() {
		sec, _ := o.obj["security"].([]any)
		var schemes []string
		for _, req := range sec {
			for name := range req.(map[string]any) {
				schemes = append(schemes, name)
			}
		}
		sort.Strings(schemes)
		id := strings.TrimPrefix(o.id, "head")
		var want []string
		switch {
		case open[id]:
		case strings.HasPrefix(o.path, "/admin/"):
			want = []string{"adminToken"}
		default:
			want = []string{"adminToken", "agentToken"}
		}
		if !slices.Equal(schemes, want) {
			t.Errorf("%s: security is %v, want %v", o.id, schemes, want)
		}
	}
	for name := range get(sp.root, "components", "securitySchemes").(map[string]any) {
		if name != "agentToken" && name != "adminToken" {
			t.Errorf("unexpected security scheme %q", name)
		}
	}
}

// ---- calling the server, and checking what comes back ----

type contract struct {
	t    *testing.T
	sp   *spec
	mu   sync.Mutex
	seen map[string]map[string]bool // operation -> status ("200", ...) seen
	dflt bool                       // a response only the catch-all covers was seen
}

func newContract(t *testing.T) *contract {
	return &contract{t: t, sp: loadSpec(t), seen: map[string]map[string]bool{}}
}

// record checks one response against the document.
func (c *contract) record(opID string, status int, hdr http.Header, body []byte, isHead, viaDefault bool) {
	c.t.Helper()
	o := c.sp.op(opID)
	if o.id == "" {
		c.t.Errorf("there is no operation %q in the document", opID)
		return
	}
	key := strconv.Itoa(status)
	resp := c.sp.response(o, key)
	if resp == nil {
		if !viaDefault {
			c.t.Errorf("%s: the server answered %d, which docs/openapi.json does not list (body %.200s)", opID, status, body)
			return
		}
		resp = c.sp.response(o, "default")
		c.mu.Lock()
		c.dflt = true
		c.mu.Unlock()
	} else {
		c.mu.Lock()
		if c.seen[opID] == nil {
			c.seen[opID] = map[string]bool{}
		}
		c.seen[opID][key] = true
		c.mu.Unlock()
	}

	// Headers: every one marked required must be there, with its constant value.
	for name, h := range get(resp, "headers").(map[string]any) {
		hm := c.sp.deref(h).(map[string]any)
		got := hdr.Get(name)
		if hm["required"] == true && got == "" {
			c.t.Errorf("%s %d: the required header %s is missing", opID, status, name)
		}
		if k, ok := get(hm, "schema", "const").(string); ok && got != "" && got != k {
			c.t.Errorf("%s %d: header %s is %q, the document says %q", opID, status, name, got, k)
		}
	}

	// Body.
	content, _ := resp["content"].(map[string]any)
	ct := strings.TrimSpace(strings.SplitN(hdr.Get("Content-Type"), ";", 2)[0])
	if isHead {
		if len(body) != 0 {
			c.t.Errorf("%s %d: a HEAD response has a body", opID, status)
		}
		return
	}
	if len(body) > 0 && len(content) > 0 {
		_, exact := content[ct]
		_, wild := content["*/*"]
		if !exact && !wild {
			c.t.Errorf("%s %d: Content-Type %q is not among the documented %v", opID, status, ct, keys(content))
		}
	}
	if len(body) > 0 && len(content) == 0 && status != 202 {
		c.t.Errorf("%s %d: the document lists no body, but one came back (%.100s)", opID, status, body)
	}
	if schema := get(content, "application/json", "schema"); schema != nil && ct == "application/json" {
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			c.t.Errorf("%s %d: the body is not JSON: %v", opID, status, err)
			return
		}
		for _, e := range c.sp.check(schema, v, opID+" "+key) {
			c.t.Error(e)
		}
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// client calls a running server.
type client struct {
	c    *contract
	base string
}

func (c *contract) on(base string) *client { return &client{c, base} }

// call sends a request and checks the response. body may be a string, nil, or
// anything else (sent as JSON, and checked against the operation's request schema).
func (cl *client) call(opID, method, path string, body any, tok string, hdr ...string) (int, http.Header, []byte) {
	return cl.do(opID, method, path, body, tok, false, hdr)
}

// callDefault is call for a response only the catch-all `default` describes.
func (cl *client) callDefault(opID, method, path string, body any, tok string, hdr ...string) (int, http.Header, []byte) {
	return cl.do(opID, method, path, body, tok, true, hdr)
}

func (cl *client) do(opID, method, path string, body any, tok string, viaDefault bool, hdr []string) (int, http.Header, []byte) {
	t := cl.c.t
	t.Helper()
	var rdr io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case string:
		rdr, ctype = strings.NewReader(b), "application/json"
	default:
		j, _ := json.Marshal(b)
		rdr, ctype = bytes.NewReader(j), "application/json"
	}
	req, err := http.NewRequestWithContext(t.Context(), method, cl.base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	cl.c.record(opID, resp.StatusCode, resp.Header, data, method == http.MethodHead, viaDefault)
	return resp.StatusCode, resp.Header, data
}

// raw sends a request that breaks off partway through its body, which a normal
// client will not do, and returns the response.
func rawBrokenBody(t *testing.T, addr, path, auth string, declared int, body string) (int, http.Header, []byte) {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: %d\r\n", path, addr, declared)
	if auth != "" {
		fmt.Fprintf(conn, "Authorization: Bearer %s\r\n", auth)
	}
	fmt.Fprintf(conn, "\r\n%s", body)
	_ = conn.(*net.TCPConn).CloseWrite()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

// ---- the failures that have to be made to happen ----

// faulty is a Store that can be told to fail, or to refuse updates as if another
// change had landed first.
type faulty struct {
	inbox.Store
	fail, conflict, pingFail bool
	mu                       sync.Mutex
}

var errBoom = errors.New("the database is on fire")

func (f *faulty) failing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail
}

func (f *faulty) set(fail, conflict, ping bool) {
	f.mu.Lock()
	f.fail, f.conflict, f.pingFail = fail, conflict, ping
	f.mu.Unlock()
}

func (f *faulty) ResolveIngestKey(ctx context.Context, k string) (*inbox.Inbox, error) {
	if f.failing() {
		return nil, errBoom
	}
	return f.Store.ResolveIngestKey(ctx, k)
}

func (f *faulty) GetInboxBySlug(ctx context.Context, s string) (*inbox.Inbox, error) {
	if f.failing() {
		return nil, errBoom
	}
	return f.Store.GetInboxBySlug(ctx, s)
}

func (f *faulty) GetInboxByTokenHash(ctx context.Context, h []byte) (*inbox.Inbox, error) {
	if f.failing() {
		return nil, errBoom
	}
	return f.Store.GetInboxByTokenHash(ctx, h)
}

func (f *faulty) ListInboxes(ctx context.Context, p inbox.ListParams) ([]*inbox.Inbox, error) {
	if f.failing() {
		return nil, errBoom
	}
	return f.Store.ListInboxes(ctx, p)
}

func (f *faulty) CreateInbox(ctx context.Context, in *inbox.Inbox) error {
	if f.failing() {
		return errBoom
	}
	return f.Store.CreateInbox(ctx, in)
}

func (f *faulty) UpdateInbox(ctx context.Context, in *inbox.Inbox) error {
	f.mu.Lock()
	conflict := f.conflict
	f.mu.Unlock()
	if conflict {
		return fmt.Errorf("%w: changed by someone else", inbox.ErrConflict)
	}
	return f.Store.UpdateInbox(ctx, in)
}

func (f *faulty) Ping(ctx context.Context) error {
	f.mu.Lock()
	p := f.pingFail
	f.mu.Unlock()
	if p {
		return errBoom
	}
	return f.Store.Ping(ctx)
}

// the statuses a HEAD operation shares with its GET: the same wrapper and
// handler produce them (a 405 is for some other method, a 500 or 503 for a
// failing store or a full queue), so forcing them on the GET covers both.
var sharedWithGet = map[string]bool{"405": true, "500": true, "503": true}

func (c *contract) requireCoverage(t *testing.T) {
	t.Helper()
	var missing []string
	for _, o := range c.sp.operations() {
		for st := range o.obj["responses"].(map[string]any) {
			if st == "default" {
				continue
			}
			if strings.HasPrefix(o.id, "head") && sharedWithGet[st] {
				continue
			}
			if !c.seen[o.id][st] {
				missing = append(missing, o.id+" "+st)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("responses docs/openapi.json lists that no test has seen (so they may not happen, or may not look as described):\n  %s", strings.Join(missing, "\n  "))
	}
	if !c.dflt {
		t.Error("no test saw a response that only the `default` entry describes")
	}
}
