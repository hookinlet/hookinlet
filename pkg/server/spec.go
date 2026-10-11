// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/hookinlet/hookinlet/internal/specvalidate"
)

// operation finds the document's description of the request being served. The
// mux has set r.Pattern by the time a handler runs. It is nil for a route the
// document does not describe (such as HEAD on a path with no HEAD operation),
// which the handlers then deal with as they always did.
func (s *Server) operation(r *http.Request) *specvalidate.Operation {
	return s.spec.Lookup(r.Method, r.Pattern)
}

// parameterMessage turns the first thing wrong into the sentence the caller sees.
func parameterMessage(vs []specvalidate.Violation) string {
	v := vs[0]
	if v.Path == "" {
		return v.Reason
	}
	return "parameter \"" + truncateName(v.Path) + "\": " + v.Reason
}

// clip cuts s to at most n bytes, on a character boundary. A field name in a
// request is the caller's, and is not trusted for length.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

func truncateName(s string) string {
	if len(s) > 64 {
		return strings.ToValidUTF8(s[:64], "") + "…"
	}
	return s
}

// checkQuery refuses a request whose query string breaks the document: a
// parameter it does not list, a repeated one, or a value of the wrong type or
// outside its range. It reports false after answering.
func (s *Server) checkQuery(w http.ResponseWriter, r *http.Request) bool {
	op := s.operation(r)
	if op == nil {
		return true
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parameter", "the query string is malformed")
		return false
	}
	if vs := op.CheckQuery(values); len(vs) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_parameter", parameterMessage(vs))
		return false
	}
	return true
}
