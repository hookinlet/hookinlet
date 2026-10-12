// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"net/http"

	"github.com/hookinlet/hookinlet/internal/ui"
)

// mountConsole serves the admin console at /admin on the surface that has the admin
// API, when there is an admin API to call and a way to tell who may call it. The
// console is static files, so it is not an operation of the OpenAPI document and does
// not go through the document's checks; it carries its own security headers.
func (s *Server) mountConsole(mux *http.ServeMux, surface surface) {
	if surface == surfacePublic || s.opts.DisableUI || s.opts.DisableAdmin || s.opts.Authenticator == nil {
		return
	}
	mux.Handle(ui.Prefix+"/", ui.Handler())
	mux.HandleFunc(ui.Prefix, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			ui.Handler().ServeHTTP(w, r) // answers 405, with the console's headers
			return
		}
		http.Redirect(w, r, ui.Prefix+"/", http.StatusMovedPermanently)
	})
}
