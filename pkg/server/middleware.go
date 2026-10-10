// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

const requestIDHeader = "X-Request-Id"

// RequestID returns the request id stored in ctx by the server, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusWriter records the response status and whether anything was written.
// Unwrap lets http.ResponseController reach the underlying writer, which SSE
// needs for flushing and per-stream deadlines.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestID assigns every request an id, taken from a well-formed incoming
// X-Request-Id header or freshly generated, and echoes it on the response.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [8]byte
	// crypto/rand.Read does not fail on supported platforms; Go crashes the
	// program rather than return an error.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// accessLog logs one line per request: method, matched route pattern, status,
// duration, and request id. It logs the route pattern, never the raw path,
// because ingest paths can carry credentials. It never logs bodies or headers.
// It must sit outside recoverer and inside requestID, and must pass r through
// unchanged so the mux's r.Pattern is visible after the handler returns.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("method", r.Method),
			slog.String("route", r.Pattern),
			slog.Int("status", sw.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	})
}

// recoverer turns a handler panic into a JSON 500 and a logged stack trace.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // net/http compares this sentinel by identity
				panic(rec)
			}
			log.LogAttrs(r.Context(), slog.LevelError, "panic in handler",
				slog.String("request_id", RequestID(r.Context())),
				slog.String("route", r.Pattern),
				slog.String("panic", fmt.Sprint(rec)),
				slog.String("stack", string(debug.Stack())),
			)
			if sw, ok := w.(*statusWriter); ok && sw.wrote {
				return // too late to change the response
			}
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}
