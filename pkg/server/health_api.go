// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
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
// answer, and a sample of the stored verifier secrets has to open with the master
// key: a server whose key is not the one its data was sealed under accepts every
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
	case errors.Is(err, ErrUnreadableSecrets):
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		return "stored verifier secrets cannot be opened with the master key; see the server log"
	default:
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready: could not check the stored secrets", slog.String("err", err.Error()))
		return "the store is not reachable"
	}
	return ""
}

// ErrUnreadableSecrets is what CheckSecrets returns, wrapped, when a stored
// verifier secret does not open with the server's master key.
var ErrUnreadableSecrets = admin.ErrUnreadableSecrets

// What readiness opens: it needs to tell a master key that opens nothing from one
// that is right, which any few secrets show, not to check every inbox's. The scan
// bound is for a store in which few inboxes have a secret.
const (
	readySecretSample = 25
	readySecretScan   = 5000
)

const (
	secretsGood    = 30 * time.Second // how long a clean check is believed
	secretsBad     = 5 * time.Second  // and one that found a wrong key: it may have been fixed
	secretsTimeout = 20 * time.Second // how long the check may take, whatever the probe's patience
)

// secretsOK checks the stored secrets, at most once in a while, however often
// readiness is asked. The check belongs to the server and not to the probe that
// happened to start it: a probe that gives up does not make the secrets look
// unreadable, and only an answer about the secrets is remembered, never a failure
// to ask.
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
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), secretsTimeout)
	defer cancel()
	r, err := s.adm.SampleSecrets(cctx, readySecretSample, readySecretScan)
	if err != nil {
		return err // could not look: say so, and look again next time
	}
	// Only a master key that opens nothing takes the server out of rotation. One inbox
	// whose secret does not open fails its own deliveries, and an operator is told which;
	// the other inboxes still work, and a load balancer should keep sending to them.
	err = nil
	if r.Systemic() {
		err = r.Err()
	} else if !r.OK() {
		s.log.LogAttrs(cctx, slog.LevelWarn, "some inboxes have a verifier secret that does not open; their deliveries will fail until it is set again",
			slog.String("detail", r.Err().Error()))
	}
	s.secretsErr, s.secretsAt = err, now
	return err
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

// CheckSecrets opens every stored verifier secret with the server's master key and
// returns an error that wraps ErrUnreadableSecrets, naming some of the inboxes, if
// any does not open. cmd/hookinlet serve runs it before listening.
func (s *Server) CheckSecrets(ctx context.Context) error {
	r, err := s.adm.CheckSecrets(ctx)
	if err != nil {
		return err
	}
	return r.Err()
}
