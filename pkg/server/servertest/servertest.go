// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package servertest holds the conformance suite for what the server does with an
// embedder's Store and Authenticator: which requests a tenant's principal scopes,
// Options.RequireTenant, the readiness check across tenants, and Options.Fallback.
//
// The suites of pkg/inboxtest check an implementation of one interface on its own.
// These behaviours belong to the server, so the suite builds a real server over the
// Store and Authenticator the embedder supplies and sends it requests. It is a
// separate package because pkg/inboxtest is imported by the server's own tests.
package servertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
	"github.com/hookinlet/hookinlet/pkg/server"
)

// Callers are the credentials the suite sends requests with. Each is the headers
// that make the embedder's Authenticator return the Principal described, usually one
// "Authorization: Bearer ..." header.
type Callers struct {
	// ReaderA and ReaderB are principals that belong to the tenants "org-a" and
	// "org-b" (they implement inbox.TenantPrincipal), are not admins, and read every
	// inbox of their own tenant. Required.
	ReaderA, ReaderB http.Header
	// AdminA is an admin that belongs to the tenant "org-a". Optional: the admin API
	// cases are skipped without it.
	AdminA http.Header
	// Operator is an admin that belongs to no tenant and reads every inbox, as the
	// shipped admin token does. Required.
	Operator http.Header
	// NoTenant is a principal that belongs to no tenant, is not an admin and says
	// yes to every inbox: the mistake Options.RequireTenant is there to catch.
	// Optional: the RequireTenant cases are skipped without it.
	NoTenant http.Header
}

// Harness is what the suite needs from an embedder. Every field but NewSecretBox is
// required, except PrepareTenant.
type Harness struct {
	// NewStore returns a new, empty Store driven by now, as for inboxtest.StoreSuite.
	// The suite closes it.
	NewStore func(t *testing.T, now func() time.Time) inbox.Store
	// PrepareTenant is called on each new Store for each of the tenants "org-a" and
	// "org-b" before the suite stores anything for it, exactly as
	// inboxtest.StoreOptions.PrepareTenant is. Nil does nothing.
	PrepareTenant func(t *testing.T, s inbox.Store, tenant string)
	// NewAuthenticator returns the Authenticator the server is built with, and the
	// credentials the suite uses, for the Store s. It is called after the suite has made
	// its inboxes in s, so credentials limited to certain inboxes can name them. The
	// inboxes are, by tenant and slug: org-a/alerts, org-b/alerts, org-b/only-b, and
	// "dflt-inbox" in the default tenant (and, for the readiness cases, org-a/hook-NNN
	// and org-b/hook-NNN).
	NewAuthenticator func(t *testing.T, s inbox.Store) (inbox.Authenticator, Callers)
	// NewSecretBox returns the SecretBox the readiness cases seal secrets with and the
	// server opens them with. Nil uses the secretbox package's, with a fixed key. The
	// suite wraps it, to make one tenant's secrets fail to open.
	NewSecretBox func(t *testing.T) inbox.SecretBox
}

// The inboxes the suite makes and what each holds.
const (
	bodyA   = "from-org-a"
	bodyB   = "from-org-b"
	bodyOnB = "only-in-b"
	bodyDef = "from-default"
)

// ServerSuite builds servers over the embedder's Store and Authenticator and checks
// what the server does with tenants. The groups are Tenants (which requests a
// TenantPrincipal scopes), RequireTenant, Readiness (the sample of stored secrets
// across tenants) and Fallback.
func ServerSuite(t *testing.T, h Harness) {
	t.Helper()
	if h.NewStore == nil || h.NewAuthenticator == nil {
		t.Fatal("servertest: Harness.NewStore and Harness.NewAuthenticator are required")
	}
	t.Run("Tenants", func(t *testing.T) { runTenants(t, h) })
	t.Run("RequireTenant", func(t *testing.T) { runRequireTenant(t, h) })
	t.Run("Readiness", func(t *testing.T) { runReadiness(t, h) })
	t.Run("Fallback", func(t *testing.T) { runFallback(t, h) })
}

// ---- the server under test ----

type fixture struct {
	t       *testing.T
	store   inbox.Store
	base    string
	callers Callers
	logs    *syncBuffer
	box     *failingBox
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// failingBox is a SecretBox whose key is gone for some inboxes, as when a key service
// has lost one organization's key. It counts the opens. It is an inbox.ContextSecretBox
// whatever it wraps, and hands the context on, so that a box that keeps a key per tenant
// by the context is checked as it works.
type failingBox struct {
	inner inbox.SecretBox
	mu    sync.Mutex
	down  map[string]bool
	opens atomic.Int64
}

func (b *failingBox) Seal(pt, binding []byte) ([]byte, error) {
	return b.SealContext(context.Background(), pt, binding)
}

func (b *failingBox) SealContext(ctx context.Context, pt, binding []byte) ([]byte, error) {
	return inbox.SealWith(ctx, b.inner, pt, binding)
}

func (b *failingBox) Open(ct, binding []byte) ([]byte, error) {
	return b.OpenContext(context.Background(), ct, binding)
}

func (b *failingBox) OpenContext(ctx context.Context, ct, binding []byte) ([]byte, error) {
	b.opens.Add(1)
	b.mu.Lock()
	down := b.down[string(binding)]
	b.mu.Unlock()
	if down {
		return nil, fmt.Errorf("no key for this tenant")
	}
	return inbox.OpenWith(ctx, b.inner, ct, binding)
}

func (b *failingBox) setDown(context []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.down[string(context)] = true
}

type config struct {
	mods  func(*server.Options)
	extra func(t *testing.T, s inbox.Store, box *failingBox) // more inboxes, made before the Authenticator
}

func newFixture(t *testing.T, h Harness, cfg config) *fixture {
	t.Helper()
	store := h.NewStore(t, time.Now)
	t.Cleanup(func() { _ = store.Close() })
	var inner inbox.SecretBox
	if h.NewSecretBox != nil {
		inner = h.NewSecretBox(t)
	} else {
		var err error
		if inner, err = secretbox.New(bytes.Repeat([]byte{7}, secretbox.KeySize)); err != nil {
			t.Fatal(err)
		}
	}
	box := &failingBox{inner: inner, down: map[string]bool{}}

	for _, tenant := range []string{"org-a", "org-b"} {
		if h.PrepareTenant != nil {
			h.PrepareTenant(t, store, tenant)
		}
	}
	for _, c := range []struct{ tenant, slug, key, body string }{
		{"org-a", "alerts", "scope-key-a", bodyA},
		{"org-b", "alerts", "scope-key-b", bodyB},
		{"org-b", "only-b", "scope-key-only-b", bodyOnB},
		{"", "dflt-inbox", "scope-key-default", bodyDef},
	} {
		in := &inbox.Inbox{Slug: c.slug, IngestKey: c.key, Name: c.slug, Verifier: "none", Tenant: c.tenant}
		if err := store.CreateInbox(t.Context(), in); err != nil {
			t.Fatalf("creating %s/%s: %v", c.tenant, c.slug, err)
		}
	}
	if cfg.extra != nil {
		cfg.extra(t, store, box)
	}
	auth, callers := h.NewAuthenticator(t, store)
	if callers.ReaderA == nil || callers.ReaderB == nil || callers.Operator == nil {
		t.Fatal("servertest: Callers.ReaderA, ReaderB and Operator are required")
	}

	f := &fixture{t: t, store: store, callers: callers, logs: &syncBuffer{}, box: box}
	opts := server.Options{
		Addr:          "127.0.0.1:0",
		Logger:        slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Store:         store,
		SecretBox:     box,
		Authenticator: auth,
	}
	if cfg.mods != nil {
		cfg.mods(&opts)
	}
	srv, err := server.New(opts)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	f.base = "http://" + ln.Addr().String()

	for _, c := range []struct{ key, body string }{
		{"scope-key-a", bodyA}, {"scope-key-b", bodyB}, {"scope-key-only-b", bodyOnB}, {"scope-key-default", bodyDef},
	} {
		if status, body := f.do(http.MethodPost, "/in/"+c.key, nil, c.body); status/100 != 2 {
			t.Fatalf("delivery to %s: %d %s", c.key, status, body)
		}
	}
	return f
}

// do sends a request and returns the status and body.
func (f *fixture) do(method, path string, hdr http.Header, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.base+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (f *fixture) get(path string, hdr http.Header) (int, string) {
	f.t.Helper()
	return f.do(http.MethodGet, path, hdr, "")
}

// stream follows a stream for up to d, and returns the status and what was read, which
// ends early once it holds until.
func (f *fixture) stream(path string, hdr http.Header, until string, d time.Duration) (int, string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("stream %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got strings.Builder
	buf := make([]byte, 4096)
	for resp.StatusCode == http.StatusOK && !strings.Contains(got.String(), until) {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		got.Write(b)
	}
	return resp.StatusCode, got.String()
}

// headers merges the credential's headers with more.
func with(cred http.Header, kv ...string) http.Header {
	h := cred.Clone()
	if h == nil {
		h = http.Header{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// ---- MCP ----

type headerTransport struct{ h http.Header }

func (r headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, vs := range r.h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (f *fixture) mcpSession(cred http.Header) *mcp.ClientSession {
	f.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "servertest", Version: "0"}, nil)
	cs, err := client.Connect(f.t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             f.base + "/mcp",
		HTTPClient:           &http.Client{Transport: headerTransport{cred}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		f.t.Fatalf("MCP connect: %v", err)
	}
	f.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// tool calls an MCP tool and returns its structured result as JSON, and whether it
// reported an error (then the JSON is the error text).
func (f *fixture) tool(cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	f.t.Helper()
	res, err := cs.CallTool(f.t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), true
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b), false
}

// slugsIn returns every "slug" value in a JSON document.
func slugsIn(t *testing.T, doc string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("not JSON: %q", doc)
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if s, ok := x["slug"].(string); ok {
				out = append(out, s)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
