// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/api"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// healthAPI serves the two health checks through the generated interface.
type healthAPI struct {
	s *Server
}

// Healthz says the process is up. It checks nothing else, so that an orchestrator
// restarts a process only when it cannot answer at all.
func (a *healthAPI) Healthz(ctx context.Context, _ api.HealthzRequestObject) (api.HealthzResponseObject, error) {
	return api.Healthz200JSONResponse{Body: api.Status{Status: "ok"}, Headers: api.Healthz200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

// Readyz reports whether the Store is reachable. Unlike Healthz it fails while a
// dependency is down.
func (a *healthAPI) Readyz(ctx context.Context, _ api.ReadyzRequestObject) (api.ReadyzResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if why := a.s.notReady(ctx); why != "" {
		var b api.ErrorNotReady
		b.Error.Code, b.Error.Message = api.NotReady, why
		return api.Readyz503JSONResponse{Body: b, Headers: api.Readyz503ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
	}
	return api.Readyz200JSONResponse{Body: api.Status{Status: "ready"}, Headers: api.Readyz200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

// HeadHealthz and HeadReadyz answer a HEAD as the GET would, without a body.
func (a *healthAPI) HeadHealthz(ctx context.Context, _ api.HeadHealthzRequestObject) (api.HeadHealthzResponseObject, error) {
	return api.HeadHealthz200Response{Headers: api.HeadHealthz200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *healthAPI) HeadReadyz(ctx context.Context, _ api.HeadReadyzRequestObject) (api.HeadReadyzResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	id := requestIDOf(ctx)
	if why := a.s.notReady(ctx); why != "" {
		return api.HeadReadyz503Response{Headers: api.HeadReadyz503ResponseHeaders{XRequestId: id}}, nil
	}
	return api.HeadReadyz200Response{Headers: api.HeadReadyz200ResponseHeaders{XRequestId: id}}, nil
}

// notReady says why the server is not ready, or returns "". The Store has to
// answer, and at least one of a sample of the stored verifier secrets has to open: a
// server whose master key is not the one its data was sealed under accepts every
// delivery and fails every one that needs a secret.
func (s *Server) notReady(ctx context.Context) string {
	if s.draining.Load() {
		return "the server is shutting down"
	}
	if err := s.opts.Store.Ping(ctx); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		if errors.Is(err, inbox.ErrStorageFull) {
			return "there is no room left to store deliveries"
		}
		return "the store is not reachable"
	}
	switch err := s.secretsOK(ctx); {
	case err == nil:
	case errors.Is(err, errSecretsCheckTimedOut):
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready: checking the stored secrets timed out; the key service may be slow or down", slog.String("err", err.Error()))
		return "checking the stored secrets timed out (the key service may be slow or down); see the server log"
	case errors.Is(err, ErrUnreadableSecrets):
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		return "none of the stored verifier secrets tried can be opened: the master key, or the key service, may be wrong; see the server log"
	default:
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready: could not check the stored secrets", slog.String("err", err.Error()))
		return "the store is not reachable"
	}
	return ""
}

// ErrUnreadableSecrets is what CheckSecrets returns, wrapped, when a stored
// verifier secret does not open with the server's master key.
var ErrUnreadableSecrets = admin.ErrUnreadableSecrets

// errSecretsCheckTimedOut is what the readiness check of the stored secrets returns,
// wrapped, when it ran out of time, which with a SecretBox that asks a key service
// means the service is slow or down. It says nothing about whether the secrets open.
var errSecretsCheckTimedOut = errors.New("checking the stored secrets timed out")

// What readiness opens: it needs to tell a master key that opens nothing from one
// that is right, which any few secrets show, not to check every inbox's. It takes at
// most readySecretPerTenant of each tenant's secrets before more of any one, so that
// one tenant whose secrets do not open (a SecretBox may keep a key per tenant, and
// lose one) cannot fill the sample and take every tenant's server out of rotation;
// one secret that opens is enough. The scan bound is for a store in which few
// inboxes have a secret. Once the sample could be filled, readiness looks at no more
// than readySecretAfterFull further inboxes for other tenants, so that a store of one
// tenant is not read to the scan bound on every check.
const (
	readySecretSample    = 25
	readySecretPerTenant = 3
	readySecretScan      = 5000
	readySecretAfterFull = 500
)

const (
	secretsGood    = 30 * time.Second // how long a clean check is believed
	secretsBad     = 5 * time.Second  // and one that found a wrong key, or timed out: it may have been fixed
	secretsTimeout = 20 * time.Second // how long the check may take, whatever the probe's patience
)

// secretsOK checks the stored secrets, at most once in a while, however often
// readiness is asked. The check belongs to the server and not to the probe that
// happened to start it: a probe that gives up does not make the secrets look
// unreadable, and only an answer about the secrets is remembered, never a failure
// to ask, except a check that ran out of time: that is remembered as briefly as a
// failing one (secretsBad), so that probes do not each wait out the timeout.
func (s *Server) secretsOK(ctx context.Context) error {
	if s.opts.SkipReadinessSecretCheck {
		return nil
	}
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	now := time.Now()
	if !s.secretsAt.IsZero() {
		ttl := secretsGood
		if s.secretsErr != nil {
			ttl = secretsBad
		}
		if now.Sub(s.secretsAt) < ttl {
			return s.secretsErr
		}
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.secretsCheckTimeout)
	defer cancel()
	r, err := s.adm.SampleSecrets(cctx, admin.SampleBounds{
		Secrets: readySecretSample, PerTenant: readySecretPerTenant, Inboxes: readySecretScan, AfterFull: readySecretAfterFull,
	})
	if err != nil && cctx.Err() != nil {
		// Out of time, most likely waiting on a key service. That is remembered as
		// briefly as a failing check, so that probes do not each wait for it in turn,
		// and is not taken for secrets that do not open.
		err = fmt.Errorf("%w after %v: %w", errSecretsCheckTimedOut, s.secretsCheckTimeout, err)
		s.secretsErr, s.secretsAt = err, now
		return err
	}
	if err != nil {
		return err // could not look: say so, and look again next time
	}
	// Only a sample in which nothing opens takes the server out of rotation. An inbox,
	// or a tenant, whose secrets do not open fails its own deliveries, and an operator is
	// told which; the others still work, and a load balancer should keep sending to them.
	err = nil
	switch {
	case r.Systemic():
		err = r.Err()
		if !r.NoKey { // the slugs, and their tenants, are in the lines below
			err = fmt.Errorf("%w: none of the %d tried opens, in %d tenant(s)", ErrUnreadableSecrets, r.Checked, len(r.Tenants))
		}
		msg := "no verifier secret in the readiness sample opens: the master key may not be the one they were sealed under, or the key service may be failing"
		if len(r.Tenants) == 1 && r.Tenants[0].Tenant != "" {
			msg += "; the sample held one tenant's secrets only, so that tenant's key alone being gone looks the same"
		}
		s.logUnreadable(cctx, msg, r)
	case !r.OK():
		s.logUnreadable(cctx, "some verifier secrets in the readiness sample do not open; deliveries to those inboxes fail until the master key or that tenant's key in the key service is put right, or the secret is set again", r)
	}
	s.secretsErr, s.secretsAt = err, now
	return err
}

// logUnreadable logs, for each tenant in r with a secret that did not open, how many
// did not of how many were tried, and a few of the inboxes' slugs. The default
// tenant's line has no tenant field. Secrets are never logged.
func (s *Server) logUnreadable(ctx context.Context, msg string, r admin.SecretReport) {
	for _, t := range r.Tenants {
		if len(t.Unreadable) == 0 {
			continue
		}
		slugs := t.Unreadable
		if len(slugs) > 5 {
			slugs = slugs[:5]
		}
		attrs := make([]slog.Attr, 0, 4)
		if t.Tenant != "" {
			attrs = append(attrs, slog.String("tenant", t.Tenant))
		}
		attrs = append(attrs, slog.Int("unreadable", len(t.Unreadable)), slog.Int("tried", t.Checked), slog.Any("slugs", slugs))
		s.log.LogAttrs(ctx, slog.LevelWarn, msg, attrs...)
	}
}

// LegacySecrets counts the stored verifier secrets that are still in the original
// sealed layout (see secretbox.Legacy): they are bound to no inbox until
// `hookinlet rekey` writes them again.
func (s *Server) LegacySecrets(ctx context.Context) int {
	r, err := s.adm.CheckSecrets(ctx)
	if err != nil {
		return 0
	}
	return r.Legacy
}

// CheckSecrets opens every stored verifier secret, of every tenant, with the
// server's SecretBox and returns an error that wraps ErrUnreadableSecrets, naming some
// of the inboxes, if any does not open. Unlike /readyz it fails for one inbox, or one
// tenant whose key is gone, as well as for a wrong master key; and it opens every
// secret, which with a SecretBox that asks a key service is one request each.
// cmd/hookinlet serve runs it before listening.
func (s *Server) CheckSecrets(ctx context.Context) error {
	r, err := s.adm.CheckSecrets(ctx)
	if err != nil {
		return err
	}
	return r.Err()
}
