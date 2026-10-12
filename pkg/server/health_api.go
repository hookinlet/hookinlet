// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/hookinlet/hookinlet/internal/api"
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
// answer, and the stored verifier secrets have to open with the master key: a
// server whose key is not the one its data was sealed under accepts every
// delivery and fails every one that needs a secret.
func (s *Server) notReady(ctx context.Context) string {
	if err := s.opts.Store.Ping(ctx); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		return "the store is not reachable"
	}
	if err := s.secretsOK(ctx); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		return "stored verifier secrets cannot be opened with the master key; see the server log"
	}
	return ""
}

const (
	secretsGood = 30 * time.Second // how long a clean check is believed
	secretsBad  = 5 * time.Second  // and a failed one: it may have been fixed
)

// secretsOK checks the stored secrets, at most once in a while, however often
// readiness is asked.
func (s *Server) secretsOK(ctx context.Context) error {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	now := s.opts.Now()
	if !s.secretsAt.IsZero() {
		ttl := secretsGood
		if s.secretsErr != nil {
			ttl = secretsBad
		}
		if now.Sub(s.secretsAt) < ttl {
			return s.secretsErr
		}
	}
	s.secretsErr = s.CheckSecrets(ctx)
	s.secretsAt = now
	return s.secretsErr
}

// CheckSecrets opens every stored verifier secret with the server's master key and
// returns an error that wraps admin.ErrUnreadableSecrets, naming some of the
// inboxes, if any does not open. cmd/hookinlet serve runs it before listening.
func (s *Server) CheckSecrets(ctx context.Context) error {
	r, err := s.adm.CheckSecrets(ctx)
	if err != nil {
		return err
	}
	return r.Err()
}
