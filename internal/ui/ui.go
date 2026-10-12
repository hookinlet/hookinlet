// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package ui serves the admin console: the single-page app built from web/ and
// embedded in the binary. The built files are committed (internal/ui/dist), so a Go
// build needs no Node; CI rebuilds them from the lockfile and fails if they differ.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Prefix is where the console is served.
const Prefix = "/admin"

// csp allows what the console loads and nothing else: its own scripts, styles, fonts
// and images, and calls to its own origin's API. No inline script or style, no frames,
// no forms, no other origin. Message bodies are written by senders, so the page that
// shows them gets the strictest policy that works.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the console at Prefix. A path that names a file is served as that
// file (or is a 404); any other path under the prefix is a screen of the app, and
// gets its index page so that a reload or a shared link works.
func Handler() http.Handler {
	files, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("ui: the embedded files are missing: " + err.Error()) // a build mistake, not a runtime one
	}
	return handler(files)
}

func handler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, Prefix)), "/")
		if name == "" {
			name = "index.html"
		}
		fi, err := fs.Stat(files, name)
		switch {
		case err == nil && fi.IsDir():
			http.NotFound(w, r) // a directory of files is not listed, and is not a screen
			return
		case err != nil && path.Ext(name) != "":
			http.NotFound(w, r) // a missing file is not a screen
			return
		case err != nil:
			name = "index.html"
		}
		if strings.HasPrefix(name, "assets/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable") // the file name carries a hash
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		http.ServeFileFS(w, r, files, name) //nolint:gosec // name is cleaned above, and an fs.FS refuses to leave its tree
	})
}
