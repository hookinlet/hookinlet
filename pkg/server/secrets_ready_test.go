// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/secretbox"
)

// A server whose master key is not the one its data was sealed under is not ready,
// and says why without saying which key.
func TestReadyzFailsWhenTheMasterKeyIsNotTheOneTheSecretsWereSealedUnder(t *testing.T) {
	e := newEnv(t)
	e.newInbox("sealed", "github", "a-secret")
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("with the right key: %d %s", rec.Code, rec.Body)
	}
	if err := e.server.CheckSecrets(t.Context()); err != nil {
		t.Fatalf("CheckSecrets with the right key: %v", err)
	}

	other := newEnv(t, func(o *Options) {
		o.SecretBox, _ = secretbox.New(bytes.Repeat([]byte{77}, secretbox.KeySize))
		o.Store = e.store // the same data, another key
	})
	err := other.server.CheckSecrets(t.Context())
	if !errors.Is(err, admin.ErrUnreadableSecrets) || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("CheckSecrets with another key: %v", err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := other.do(req{method: method, path: "/readyz"})
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s /readyz with another key: %d", method, rec.Code)
		}
		if method == http.MethodGet && (errorCode(t, rec) != "not_ready" || strings.Contains(rec.Body.String(), "sealed")) {
			t.Errorf("the answer: %s", rec.Body)
		}
	}
	// Liveness is not readiness: the process is up.
	if rec := other.do(req{method: http.MethodGet, path: "/healthz"}); rec.Code != http.StatusOK {
		t.Errorf("/healthz: %d", rec.Code)
	}
}

// The check is not repeated on every probe, and a failure is rechecked soon.
func TestReadyzDoesNotOpenEverySecretOnEveryProbe(t *testing.T) {
	e := newEnv(t)
	e.newInbox("sealed", "github", "a-secret")
	counter := &secretListCounter{Store: e.store}
	e.server.opts.Store = counter
	e.server.adm.Store = counter
	for range 5 {
		e.do(req{method: http.MethodGet, path: "/readyz"})
	}
	if counter.n != 1 {
		t.Errorf("%d listings for five probes, want 1", counter.n)
	}
	e.server.secretsMu.Lock()
	e.server.secretsAt = e.server.secretsAt.Add(-time.Minute) // a minute passes
	e.server.secretsMu.Unlock()
	e.do(req{method: http.MethodGet, path: "/readyz"})
	if counter.n != 2 {
		t.Errorf("%d listings a minute later, want 2", counter.n)
	}
}

type secretListCounter struct {
	inbox.Store
	n int
}

func (l *secretListCounter) ListInboxes(ctx2 context.Context, p inbox.ListParams) ([]*inbox.Inbox, error) {
	l.n++
	return l.Store.ListInboxes(ctx2, p)
}

// A probe that gives up does not make the secrets look unreadable, and the failure
// to ask is not remembered as an answer.
func TestAProbeThatGivesUpDoesNotPoisonTheReadinessCheck(t *testing.T) {
	e := newEnv(t)
	e.newInbox("sealed", "github", "a-secret")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := e.server.secretsOK(cancelled); err != nil {
		t.Fatalf("the check ran under the probe's context: %v", err)
	}
	if err := e.server.secretsOK(t.Context()); err != nil {
		t.Errorf("a later probe: %v", err)
	}

	failing := newEnv(t)
	failing.newInbox("sealed", "github", "a-secret")
	failing.server.adm.Store = failingList{failing.store}
	if err := failing.server.secretsOK(t.Context()); err == nil || errors.Is(err, ErrUnreadableSecrets) {
		t.Fatalf("a store that cannot list: %v", err)
	}
	failing.server.adm.Store = failing.store
	if err := failing.server.secretsOK(t.Context()); err != nil {
		t.Errorf("after the store recovered: %v", err)
	}
	if rec := failing.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Errorf("readyz: %d", rec.Code)
	}
}

type failingList struct{ inbox.Store }

func (failingList) ListInboxes(context.Context, inbox.ListParams) ([]*inbox.Inbox, error) {
	return nil, errors.New("boom")
}

// One inbox whose secret does not open is that inbox's problem: the server stays ready
// for the others, while starting still refuses so that an operator sees it.
func TestReadyzStaysUpWhenOnlyOneInboxHasASecretThatDoesNotOpen(t *testing.T) {
	e := newEnv(t)
	e.newInbox("good", "github", "a-secret")
	foreign, err := secretbox.New(bytes.Repeat([]byte{77}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	uid, err := inbox.NewUID()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := foreign.Seal([]byte("another-secret"), inbox.VerifierSecretContext(uid))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateInbox(t.Context(), &inbox.Inbox{Slug: "foreign", UID: uid, IngestKey: "foreign-key", Name: "f", Verifier: "github", VerifierSecret: sealed}); err != nil {
		t.Fatal(err)
	}

	if err := e.server.CheckSecrets(t.Context()); !errors.Is(err, admin.ErrUnreadableSecrets) || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("CheckSecrets with one foreign secret: %v", err)
	}
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Errorf("/readyz with one unreadable secret among readable ones: %d %s", rec.Code, rec.Body)
	}
}

// openCounter counts how many secrets are opened.
type openCounter struct {
	inbox.SecretBox
	n int
}

func (c *openCounter) Open(ct, context []byte) ([]byte, error) {
	c.n++
	return c.SecretBox.Open(ct, context)
}

// Readiness answers whether the master key is right, which a few secrets show, so what
// it costs does not grow with the number of inboxes.
func TestReadyzOpensOnlyASampleOfTheSecrets(t *testing.T) {
	e := newEnv(t)
	for i := range 3 * readySecretSample {
		e.newInbox(fmt.Sprintf("sealed-%03d", i), "github", "a-secret")
	}
	counter := &openCounter{SecretBox: e.box}
	e.server.adm.Box = counter
	if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Fatalf("/readyz: %d %s", rec.Code, rec.Body)
	}
	if counter.n != readySecretSample {
		t.Errorf("readiness opened %d secrets of %d, want %d", counter.n, 3*readySecretSample, readySecretSample)
	}
	// Starting still checks them all.
	counter.n = 0
	if err := e.server.CheckSecrets(t.Context()); err != nil || counter.n != 3*readySecretSample {
		t.Errorf("CheckSecrets opened %d secrets (%v), want all %d", counter.n, err, 3*readySecretSample)
	}
}

// A wrong key shows in the sample, however many inboxes there are.
func TestReadyzSeesAWrongKeyInTheSample(t *testing.T) {
	e := newEnv(t)
	for i := range 3 * readySecretSample {
		e.newInbox(fmt.Sprintf("sealed-%03d", i), "github", "a-secret")
	}
	other := newEnv(t, func(o *Options) {
		o.SecretBox, _ = secretbox.New(bytes.Repeat([]byte{77}, secretbox.KeySize))
		o.Store = e.store
	})
	if rec := other.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with another key: %d", rec.Code)
	}
}

// An embedder whose SecretBox goes to a key service has its own check of that, and can
// keep readiness from opening anything.
func TestReadyzCanSkipTheSecretsCheck(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.SkipReadinessSecretCheck = true })
	e.newInbox("sealed", "github", "a-secret")
	counter := &openCounter{SecretBox: e.box}
	e.server.adm.Box = counter
	for range 3 {
		if rec := e.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
			t.Fatalf("/readyz: %d %s", rec.Code, rec.Body)
		}
	}
	if counter.n != 0 {
		t.Errorf("readiness opened %d secrets with the check skipped", counter.n)
	}
	// Not even a wrong key is looked for, and the Store still has to answer.
	wrong := newEnv(t, func(o *Options) {
		o.SkipReadinessSecretCheck = true
		o.SecretBox, _ = secretbox.New(bytes.Repeat([]byte{77}, secretbox.KeySize))
		o.Store = e.store
	})
	if rec := wrong.do(req{method: http.MethodGet, path: "/readyz"}); rec.Code != http.StatusOK {
		t.Errorf("/readyz with the check skipped and another key: %d", rec.Code)
	}
}
