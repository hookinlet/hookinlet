// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

// boxCall is one call to the box: its context, and the UID of the inbox whose secret
// it was, from the binding.
type boxCall struct {
	ctx context.Context
	uid string
}

// scope is what inbox.TenantFrom reports.
type scope struct {
	tenant string
	scoped bool
}

func scopeOf(ctx context.Context) scope {
	t, ok := inbox.TenantFrom(ctx)
	return scope{t, ok}
}

// ctxRecBox is the env's box with the context methods: it records every seal and
// open, and can be made to wait in OpenContext until its context ends, like a box
// whose key service does not answer.
type ctxRecBox struct {
	*secretbox.Box
	mu       sync.Mutex
	seals    []boxCall
	opens    []boxCall
	plain    int           // calls to Seal or Open, which the server must not make
	block    bool          // OpenContext waits for ctx.Done
	once     sync.Once     // closes entered
	entered  chan struct{} // closed when a blocking OpenContext first starts waiting
	returned chan error    // what the first blocking OpenContext returned
}

func newCtxRecBox(b *secretbox.Box) *ctxRecBox {
	return &ctxRecBox{Box: b, entered: make(chan struct{}), returned: make(chan error, 1)}
}

func uidOf(binding []byte) string {
	return strings.TrimPrefix(string(binding), string(inbox.VerifierSecretContext("")))
}

func (b *ctxRecBox) Seal(pt, binding []byte) ([]byte, error) {
	b.mu.Lock()
	b.plain++
	b.mu.Unlock()
	return b.Box.Seal(pt, binding)
}

func (b *ctxRecBox) Open(ct, binding []byte) ([]byte, error) {
	b.mu.Lock()
	b.plain++
	b.mu.Unlock()
	return b.Box.Open(ct, binding)
}

func (b *ctxRecBox) SealContext(ctx context.Context, pt, binding []byte) ([]byte, error) {
	b.mu.Lock()
	b.seals = append(b.seals, boxCall{ctx, uidOf(binding)})
	b.mu.Unlock()
	return b.Box.Seal(pt, binding)
}

func (b *ctxRecBox) setBlock(v bool) {
	b.mu.Lock()
	b.block = v
	b.mu.Unlock()
}

func (b *ctxRecBox) OpenContext(ctx context.Context, ct, binding []byte) ([]byte, error) {
	b.mu.Lock()
	b.opens = append(b.opens, boxCall{ctx, uidOf(binding)})
	block := b.block
	b.mu.Unlock()
	if block {
		b.once.Do(func() { close(b.entered) })
		var err error
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(30 * time.Second):
			err = errors.New("the key service never answered")
		}
		select {
		case b.returned <- err:
		default:
		}
		return nil, err
	}
	return b.Box.Open(ct, binding)
}

func (b *ctxRecBox) recorded() (seals, opens []boxCall, plain int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]boxCall(nil), b.seals...), append([]boxCall(nil), b.opens...), b.plain
}

var _ inbox.ContextSecretBox = (*ctxRecBox)(nil)

func ctxBoxEnv(t *testing.T, mods ...func(*Options)) (*env, *ctxRecBox) {
	t.Helper()
	var box *ctxRecBox
	e := newEnv(t, append([]func(*Options){func(o *Options) {
		box = newCtxRecBox(o.SecretBox.(*secretbox.Box))
		o.SecretBox = box
	}}, mods...)...)
	return e, box
}

// ctxRecStore records the context of the Store calls on the paths that open or seal
// a secret, so a test can check that giving the box a tenant left them alone.
type ctxRecStore struct {
	inbox.Store
	mu    sync.Mutex
	calls map[string][]scope
}

func (s *ctxRecStore) rec(name string, ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[name] = append(s.calls[name], scopeOf(ctx))
}

func (s *ctxRecStore) scopes(name string) []scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scope(nil), s.calls[name]...)
}

func (s *ctxRecStore) ResolveIngestKey(ctx context.Context, key string) (*inbox.Inbox, error) {
	s.rec("ResolveIngestKey", ctx)
	return s.Store.ResolveIngestKey(ctx, key)
}

func (s *ctxRecStore) Append(ctx context.Context, id inbox.ID, m *inbox.NewMessage) (*inbox.Message, bool, error) {
	s.rec("Append", ctx)
	return s.Store.Append(ctx, id, m)
}

func (s *ctxRecStore) ListInboxes(ctx context.Context, p inbox.ListParams) ([]*inbox.Inbox, error) {
	s.rec("ListInboxes", ctx)
	return s.Store.ListInboxes(ctx, p)
}

func (s *ctxRecStore) GetInboxBySlug(ctx context.Context, slug string) (*inbox.Inbox, error) {
	s.rec("GetInboxBySlug", ctx)
	return s.Store.GetInboxBySlug(ctx, slug)
}

func (s *ctxRecStore) CreateInbox(ctx context.Context, in *inbox.Inbox) error {
	s.rec("CreateInbox", ctx)
	return s.Store.CreateInbox(ctx, in)
}

func (s *ctxRecStore) UpdateInbox(ctx context.Context, in *inbox.Inbox) error {
	s.rec("UpdateInbox", ctx)
	return s.Store.UpdateInbox(ctx, in)
}

// recordStore puts a ctxRecStore in front of the env's store.
func recordStore(e *env) *ctxRecStore {
	rs := &ctxRecStore{Store: e.store, calls: map[string][]scope{}}
	e.server.opts.Store = rs
	e.server.adm.Store = rs
	return rs
}

// threeTenants makes an inbox with a secret in each of org-a, org-b and the default
// tenant, and returns them by tenant.
func threeTenants(e *env) map[string]*inbox.Inbox {
	out := map[string]*inbox.Inbox{}
	for _, c := range []struct{ tenant, slug string }{{"org-a", "in-a"}, {"org-b", "in-b"}, {"", "in-d"}} {
		out[c.tenant] = e.newInbox(c.slug, "github", "secret-"+c.slug, func(in *inbox.Inbox) { in.Tenant = c.tenant })
	}
	return out
}

// checkTenants checks that each call was for one of ins and that its context was
// scoped to that inbox's tenant, the default one included, and that each of ins was
// seen.
func checkTenants(t *testing.T, what string, calls []boxCall, ins map[string]*inbox.Inbox) {
	t.Helper()
	byUID := map[string]*inbox.Inbox{}
	for _, in := range ins {
		byUID[in.UID] = in
	}
	seen := map[string]bool{}
	for _, c := range calls {
		in, ok := byUID[c.uid]
		if !ok {
			t.Errorf("%s: a call for an unknown inbox %q", what, c.uid)
			continue
		}
		seen[in.UID] = true
		if got, want := scopeOf(c.ctx), (scope{in.Tenant, true}); got != want {
			t.Errorf("%s of %s: the box's context is %+v, want %+v", what, in.Slug, got, want)
		}
	}
	if len(seen) != len(byUID) {
		t.Errorf("%s: %d of %d inboxes seen", what, len(seen), len(byUID))
	}
}

func checkUnscoped(t *testing.T, what string, scopes []scope) {
	t.Helper()
	if len(scopes) == 0 {
		t.Errorf("%s was not called", what)
	}
	for _, sc := range scopes {
		if sc.scoped {
			t.Errorf("%s was called scoped to %q; it must stay unscoped", what, sc.tenant)
		}
	}
}

// A delivery opens the inbox's secret with the request's context, given to the box
// with the inbox's tenant (the default tenant as ""), while the Store is still asked
// unscoped, as for every delivery.
func TestADeliveryOpensItsSecretWithTheRequestContextAndItsInboxsTenant(t *testing.T) {
	e, box := ctxBoxEnv(t)
	ins := threeTenants(e)
	rs := recordStore(e)
	for _, in := range ins {
		body := `{"zen":"x"}`
		rec := e.post("/in/"+in.IngestKey, body, "X-Hub-Signature-256", githubSig("secret-"+in.Slug, body))
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("delivery to %s: %d %s", in.Slug, rec.Code, rec.Body)
		}
	}
	_, opens, plain := box.recorded()
	if len(opens) != 3 || plain != 0 {
		t.Fatalf("%d context opens and %d plain calls, want 3 and 0", len(opens), plain)
	}
	for _, c := range opens {
		if c.ctx.Value(requestKey) == nil {
			t.Error("the context is not the request's")
		}
	}
	checkTenants(t, "delivery", opens, ins)
	checkUnscoped(t, "ResolveIngestKey", rs.scopes("ResolveIngestKey"))
	checkUnscoped(t, "Append", rs.scopes("Append"))
}

// A sender that goes away while the key service is slow cancels the open, and the
// server stops waiting for it.
func TestASenderThatGoesAwayCancelsASlowSecretOpen(t *testing.T) {
	e, box := ctxBoxEnv(t)
	box.setBlock(true)
	e.newInbox("slow", "github", "gh-secret")
	base := serveOn(t, e)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/in/slow", strings.NewReader(`{}`))
		if err != nil {
			sent <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		sent <- err
	}()
	select {
	case <-box.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery never reached OpenContext")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-box.returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("OpenContext ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenContext did not see the sender go away")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the open took %v to stop after the sender went away", d)
	}
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Errorf("the client: %v", err)
	}
}

// The admin API seals with the request's context, given to the box with the
// inbox's tenant: the caller's for a TenantPrincipal, and the default tenant ("")
// for an unscoped caller. The Store is asked as before.
func TestAnAdminSecretIsSealedWithTheCallersContextAndItsInboxsTenant(t *testing.T) {
	e, box := ctxBoxEnv(t, func(o *Options) { o.Authenticator = tenantAuth{} })
	ins := map[string]*inbox.Inbox{
		"org-a": e.newInbox("in-a", "github", "", func(in *inbox.Inbox) { in.Tenant = "org-a" }),
		"":      e.newInbox("in-d", "github", ""),
	}
	rs := recordStore(e)
	put := func(slug, tok string) {
		t.Helper()
		rec := e.do(req{method: http.MethodPut, path: "/v1/admin/inboxes/" + slug + "/secret", body: `{"secret":"new-secret"}`,
			headers: http.Header{"Authorization": {"Bearer " + tok}, "Content-Type": {"application/json"}}})
		if rec.Code != http.StatusOK {
			t.Fatalf("set secret of %s as %s: %d %s", slug, tok, rec.Code, rec.Body)
		}
	}
	put("in-a", "admin-a")
	put("in-d", "operator")
	seals, _, plain := box.recorded()
	if len(seals) != 2 || plain != 0 {
		t.Fatalf("%d context seals and %d plain calls, want 2 and 0", len(seals), plain)
	}
	for _, c := range seals {
		if c.ctx.Value(requestKey) == nil {
			t.Error("the context is not the request's")
		}
	}
	checkTenants(t, "set secret", seals, ins)
	if got, want := rs.scopes("GetInboxBySlug"), []scope{{"org-a", true}, {"", false}}; !slices.Equal(got, want) {
		t.Errorf("GetInboxBySlug was asked with %+v, want %+v", got, want)
	}

	// An inbox made with a secret: sealed with the tenant it is made in, which is the
	// tenant the box is given when a delivery opens it.
	rec := e.do(req{method: http.MethodPost, path: "/v1/admin/inboxes", body: `{"slug":"fresh","verifier":"github","verifier_secret":"fresh-secret"}`,
		headers: http.Header{"Authorization": {"Bearer admin-b"}, "Content-Type": {"application/json"}}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Inbox struct {
			IngestPath string `json:"ingest_path"`
		} `json:"inbox"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	body := `{"zen":"x"}`
	if rec := e.post(created.Inbox.IngestPath, body, "X-Hub-Signature-256", githubSig("fresh-secret", body)); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("delivery to the new inbox: %d %s", rec.Code, rec.Body)
	}
	seals, opens, _ := box.recorded()
	sealed, opened := seals[len(seals)-1], opens[len(opens)-1]
	if sealed.uid != opened.uid {
		t.Fatalf("the last seal and open are of different inboxes")
	}
	if got := scopeOf(sealed.ctx); got != (scope{"org-b", true}) {
		t.Errorf("create sealed with %+v, want org-b", got)
	}
	if scopeOf(opened.ctx) != scopeOf(sealed.ctx) {
		t.Errorf("sealed with %+v and opened with %+v", scopeOf(sealed.ctx), scopeOf(opened.ctx))
	}
	// An operator, in no tenant, makes the inbox in the default tenant: the box is
	// given "", and the Store is still asked unscoped.
	rec = e.do(req{method: http.MethodPost, path: "/v1/admin/inboxes", body: `{"slug":"fresh-d","verifier":"github","verifier_secret":"d-secret"}`,
		headers: http.Header{"Authorization": {"Bearer operator"}, "Content-Type": {"application/json"}}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create as the operator: %d %s", rec.Code, rec.Body)
	}
	seals, _, _ = box.recorded()
	if got := scopeOf(seals[len(seals)-1].ctx); got != (scope{"", true}) {
		t.Errorf("the operator's create sealed with %+v, want the default tenant", got)
	}
	if got, want := rs.scopes("CreateInbox"), []scope{{"org-b", true}, {"", false}}; !slices.Equal(got, want) {
		t.Errorf("CreateInbox was asked with %+v, want %+v", got, want)
	}
}

// Readiness opens its sample with its own context, which has a deadline, given to
// the box with each inbox's tenant. The Store is listed unscoped, as before.
func TestReadinessOpensItsSampleWithTheChecksContextAndEachInboxsTenant(t *testing.T) {
	e, box := ctxBoxEnv(t)
	ins := threeTenants(e)
	rs := recordStore(e)
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("/readyz: %d %s", rec.Code, rec.Body)
	}
	_, opens, plain := box.recorded()
	if len(opens) != 3 || plain != 0 {
		t.Fatalf("%d context opens and %d plain calls, want 3 and 0", len(opens), plain)
	}
	for _, c := range opens {
		if dl, ok := c.ctx.Deadline(); !ok || time.Until(dl) > secretsTimeout {
			t.Errorf("deadline %v (%v), want one within %v", dl, ok, secretsTimeout)
		}
	}
	checkTenants(t, "readiness", opens, ins)
	checkUnscoped(t, "ListInboxes", rs.scopes("ListInboxes"))
}

// Server.CheckSecrets opens every secret with the caller's context, given to the box
// with each inbox's tenant. The Store is listed unscoped, as before.
func TestCheckSecretsOpensWithTheCallersContextAndEachInboxsTenant(t *testing.T) {
	e, box := ctxBoxEnv(t)
	ins := threeTenants(e)
	rs := recordStore(e)
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "caller")
	if err := e.server.CheckSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	_, opens, plain := box.recorded()
	if len(opens) != 3 || plain != 0 {
		t.Fatalf("%d context opens and %d plain calls, want 3 and 0", len(opens), plain)
	}
	for _, c := range opens {
		if c.ctx.Value(key{}) != "caller" {
			t.Error("an open was not given the caller's context")
		}
	}
	checkTenants(t, "CheckSecrets", opens, ins)
	checkUnscoped(t, "ListInboxes", rs.scopes("ListInboxes"))
}

// A readiness check cut short by its deadline, as when the key service is slow or
// down, answers 503 with a reason that says so, and logs it. It is remembered as
// briefly as a failing check, and not as secrets that do not open.
func TestReadinessSaysWhenCheckingTheSecretsTimedOut(t *testing.T) {
	e, box := ctxBoxEnv(t)
	e.server.secretsCheckTimeout = 200 * time.Millisecond
	e.newInbox("slow", "github", "a-secret")
	box.setBlock(true)
	rec := e.do(req{method: http.MethodGet, path: "/readyz"})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "timed out") || !strings.Contains(rec.Body.String(), "key service") {
		t.Fatalf("/readyz with a key service that does not answer: %d %s", rec.Code, rec.Body)
	}
	if err := <-box.returned; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("OpenContext ended with %v, want context.DeadlineExceeded", err)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, "timed out") {
		t.Errorf("no warning that the check timed out:\n%s", logs)
	}
	if strings.Contains(logs, "a-secret") {
		t.Error("the log holds the secret")
	}
	e.server.secretsMu.Lock()
	cached := e.server.secretsErr
	e.server.secretsMu.Unlock()
	if errors.Is(cached, ErrUnreadableSecrets) {
		t.Errorf("a check that timed out was remembered as unreadable secrets: %v", cached)
	}

	// Asked again at once, the answer is the one remembered, without another wait.
	box.setBlock(false)
	_, before, _ := box.recorded()
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz at once: %d %s", rec.Code, rec.Body)
	}
	if _, after, _ := box.recorded(); len(after) != len(before) {
		t.Errorf("the check was repeated at once")
	}
	// Once that has passed (as long as for a failing check), the healthy box is asked
	// again and the server is ready.
	e.server.secretsMu.Lock()
	e.server.secretsAt = e.server.secretsAt.Add(-secretsBad)
	e.server.secretsMu.Unlock()
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Errorf("/readyz with the key service back: %d %s", rec.Code, rec.Body)
	}
}

// A check whose context ends while a secret is being opened did not find out
// whether the secrets open: it is an error, not a report of unreadable secrets.
func TestACheckCutShortIsNotAnUnreadableSecret(t *testing.T) {
	e, box := ctxBoxEnv(t)
	box.setBlock(true)
	e.newInbox("slow", "github", "a-secret")
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-box.entered; cancel() }()
	err := e.server.CheckSecrets(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, admin.ErrUnreadableSecrets) {
		t.Errorf("CheckSecrets: %v, want context.Canceled and not ErrUnreadableSecrets", err)
	}
}

// plainOnlyBox is a SecretBox with Seal and Open and nothing else, as every
// SecretBox was before ContextSecretBox.
type plainOnlyBox struct {
	b            *secretbox.Box
	seals, opens atomic.Int64
}

func (p *plainOnlyBox) Seal(pt, binding []byte) ([]byte, error) {
	p.seals.Add(1)
	return p.b.Seal(pt, binding)
}

func (p *plainOnlyBox) Open(ct, binding []byte) ([]byte, error) {
	p.opens.Add(1)
	return p.b.Open(ct, binding)
}

// A SecretBox without the context methods is called as before, on every path.
func TestASecretBoxWithoutTheContextMethodsWorksAsBefore(t *testing.T) {
	var box *plainOnlyBox
	e := newEnv(t, func(o *Options) {
		box = &plainOnlyBox{b: o.SecretBox.(*secretbox.Box)}
		o.SecretBox = box
	})
	if _, ok := e.server.opts.SecretBox.(inbox.ContextSecretBox); ok {
		t.Fatal("the test box has the context methods")
	}
	e.newInbox("plain", "github", "gh-secret")
	body := `{"zen":"x"}`
	if rec := e.post("/in/plain", body, "X-Hub-Signature-256", githubSig("gh-secret", body)); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("delivery: %d %s", rec.Code, rec.Body)
	}
	if _, err := e.server.adm.SetSecret(t.Context(), "plain", "other-secret"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if rec := e.post("/in/plain", body, "X-Hub-Signature-256", githubSig("other-secret", body)); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("delivery under the new secret: %d %s", rec.Code, rec.Body)
	}
	if err := e.server.CheckSecrets(t.Context()); err != nil {
		t.Fatalf("CheckSecrets: %v", err)
	}
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("/readyz: %d %s", rec.Code, rec.Body)
	}
	if s, o := box.seals.Load(), box.opens.Load(); s != 1 || o != 4 {
		t.Errorf("%d seals and %d opens, want 1 and 4 (two deliveries, CheckSecrets, readiness)", s, o)
	}
}
