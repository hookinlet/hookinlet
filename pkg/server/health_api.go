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
	if err := a.s.opts.Store.Ping(ctx); err != nil {
		a.s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		var b api.ErrorNotReady
		b.Error.Code, b.Error.Message = api.NotReady, "the store is not reachable"
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
	if err := a.s.opts.Store.Ping(ctx); err != nil {
		a.s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		return api.HeadReadyz503Response{Headers: api.HeadReadyz503ResponseHeaders{XRequestId: id}}, nil
	}
	return api.HeadReadyz200Response{Headers: api.HeadReadyz200ResponseHeaders{XRequestId: id}}, nil
}
