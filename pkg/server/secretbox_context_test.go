// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

// ctxRecBox is the env's box with the context methods: it records the context of
// every seal and open, and can be made to wait in OpenContext until its context
// ends, like a box whose key service does not answer.
type ctxRecBox struct {
	*secretbox.Box
	mu       sync.Mutex
	seals    []context.Context
	opens    []context.Context
	plain    int           // calls to Seal or Open, which the server must not make
	block    bool          // OpenContext waits for ctx.Done
	entered  chan struct{} // closed when a blocking OpenContext starts waiting
	returned chan error    // what a blocking OpenContext returned
}

func newCtxRecBox(b *secretbox.Box) *ctxRecBox {
	return &ctxRecBox{Box: b, entered: make(chan struct{}), returned: make(chan error, 1)}
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
	b.seals = append(b.seals, ctx)
	b.mu.Unlock()
	return b.Box.Seal(pt, binding)
}

func (b *ctxRecBox) OpenContext(ctx context.Context, ct, binding []byte) ([]byte, error) {
	b.mu.Lock()
	b.opens = append(b.opens, ctx)
	block := b.block
	b.mu.Unlock()
	if block {
		close(b.entered)
		select {
		case <-ctx.Done():
			err := ctx.Err()
			b.returned <- err
			return nil, err
		case <-time.After(30 * time.Second):
			b.returned <- nil
			return nil, errors.New("the key service never answered")
		}
	}
	return b.Box.Open(ct, binding)
}

func (b *ctxRecBox) recorded() (seals, opens []context.Context, plain int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]context.Context(nil), b.seals...), append([]context.Context(nil), b.opens...), b.plain
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

// A delivery opens the inbox's secret with the request's context, which is not
// scoped to a tenant even when the inbox is in one.
func TestADeliveryOpensItsSecretWithTheRequestContext(t *testing.T) {
	e, box := ctxBoxEnv(t)
	e.newInbox("tenanted", "github", "gh-secret", func(in *inbox.Inbox) { in.Tenant = "org-a" })
	body := `{"zen":"x"}`
	rec := e.post("/in/tenanted", body, "X-Hub-Signature-256", githubSig("gh-secret", body))
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("delivery: %d %s", rec.Code, rec.Body)
	}
	_, opens, plain := box.recorded()
	if len(opens) != 1 || plain != 0 {
		t.Fatalf("%d context opens and %d plain calls, want 1 and 0", len(opens), plain)
	}
	if opens[0].Value(requestKey) == nil {
		t.Error("the context is not the request's")
	}
	if tenant, scoped := inbox.TenantFrom(opens[0]); scoped {
		t.Errorf("the delivery's context is scoped to %q; deliveries are not scoped", tenant)
	}
}

// A sender that goes away while the key service is slow cancels the open, and the
// server stops waiting for it.
func TestASenderThatGoesAwayCancelsASlowSecretOpen(t *testing.T) {
	e, box := ctxBoxEnv(t)
	box.block = true
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

// Setting a secret through the admin API seals it with the request's context,
// scoped to the caller's tenant.
func TestAnAdminSecretIsSealedWithTheCallersContext(t *testing.T) {
	e, box := ctxBoxEnv(t, func(o *Options) { o.Authenticator = tenantAuth{} })
	uid, err := inbox.NewUID()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateInbox(inbox.WithTenant(t.Context(), "org-a"), &inbox.Inbox{
		Slug: "ghx", UID: uid, IngestKey: "gh-key", Name: "gh", Verifier: "github",
	}); err != nil {
		t.Fatal(err)
	}
	rec := e.do(req{method: http.MethodPut, path: "/v1/admin/inboxes/ghx/secret", body: `{"secret":"new-secret"}`,
		headers: http.Header{"Authorization": {"Bearer admin-a"}, "Content-Type": {"application/json"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set secret: %d %s", rec.Code, rec.Body)
	}
	seals, _, plain := box.recorded()
	if len(seals) != 1 || plain != 0 {
		t.Fatalf("%d context seals and %d plain calls, want 1 and 0", len(seals), plain)
	}
	if seals[0].Value(requestKey) == nil {
		t.Error("the context is not the request's")
	}
	if tenant, scoped := inbox.TenantFrom(seals[0]); !scoped || tenant != "org-a" {
		t.Errorf("the context is scoped to %q (%v), want org-a", tenant, scoped)
	}
}

// Readiness opens its sample with its own context, which has a deadline and no
// tenant.
func TestReadinessOpensItsSampleWithTheChecksContext(t *testing.T) {
	e, box := ctxBoxEnv(t)
	e.newInbox("sealed", "github", "a-secret", func(in *inbox.Inbox) { in.Tenant = "org-a" })
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("/readyz: %d %s", rec.Code, rec.Body)
	}
	_, opens, plain := box.recorded()
	if len(opens) != 1 || plain != 0 {
		t.Fatalf("%d context opens and %d plain calls, want 1 and 0", len(opens), plain)
	}
	if dl, ok := opens[0].Deadline(); !ok || time.Until(dl) > secretsTimeout {
		t.Errorf("deadline %v (%v), want one within %v", dl, ok, secretsTimeout)
	}
	if _, scoped := inbox.TenantFrom(opens[0]); scoped {
		t.Error("the readiness check's context is scoped to a tenant")
	}
}

// Server.CheckSecrets opens every secret with the caller's context.
func TestCheckSecretsOpensWithTheCallersContext(t *testing.T) {
	e, box := ctxBoxEnv(t)
	e.newInbox("one", "github", "a-secret")
	e.newInbox("two", "github", "b-secret")
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "caller")
	if err := e.server.CheckSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	_, opens, plain := box.recorded()
	if len(opens) != 2 || plain != 0 {
		t.Fatalf("%d context opens and %d plain calls, want 2 and 0", len(opens), plain)
	}
	for _, c := range opens {
		if c.Value(key{}) != "caller" {
			t.Error("an open was not given the caller's context")
		}
	}
}

// A check whose context ends while a secret is being opened did not find out
// whether the secrets open: it is an error, not a report of unreadable secrets.
func TestACheckCutShortIsNotAnUnreadableSecret(t *testing.T) {
	e, box := ctxBoxEnv(t)
	box.block = true
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
