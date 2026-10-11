// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/hookinlet/hookinlet/internal/api"
)

// strictServer is the generated handler for an implementation, without the
// binding of path parameters: for a route whose parameters are taken from the
// request as they are (the ingest key and the rest of its path are the sender's
// to choose, and an empty rest is valid).
func (s *Server) strictServer(impl api.StrictServerInterface) api.ServerInterface {
	opts := api.StrictHTTPServerOptions{
		// The checks before the handler make these unreachable for a request the
		// document forbids; they answer the rest in the usual shape.
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeError(w, http.StatusBadRequest, "invalid_request", "the request is not valid")
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			s.log.LogAttrs(r.Context(), slog.LevelError, "response failed", slog.String("err", err.Error()))
			// If the status is already sent (a download that broke part way), all that
			// can be done is to say so and let the connection break, which tells the
			// client the body is not whole.
			if sw, ok := w.(interface{ Wrote() bool }); ok && sw.Wrote() {
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		},
	}
	return api.NewStrictHandlerWithOptions(impl, nil, opts)
}
