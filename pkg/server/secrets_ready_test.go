// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"errors"
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
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := newEnv(t, func(o *Options) { o.Now = func() time.Time { return now } })
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
	now = now.Add(time.Minute)
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
