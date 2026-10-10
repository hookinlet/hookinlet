// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package server assembles hookinlet's HTTP surface. Handlers reach storage,
// notification, auth, and secrets only through interfaces in pkg/inbox, so an
// embedder can swap any of them.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// Options configures a Server. Addr and Store are required; every other field
// has a working default.
type Options struct {
	// Addr is the TCP address to listen on, for example ":8080".
	Addr string
	// Logger receives all server logs. Defaults to slog.Default().
	Logger *slog.Logger
	// ShutdownTimeout bounds how long in-flight requests get to finish after
	// the context passed to Run is cancelled. Defaults to 10s.
	ShutdownTimeout time.Duration

	// Store persists inboxes and messages. Required.
	Store inbox.Store
	// SecretBox opens sealed verifier secrets. It may be nil only if no inbox
	// has a verifier secret; a delivery to an inbox that does fails with a
	// server error.
	SecretBox inbox.SecretBox
	// Notifier is told after each newly stored message. Defaults to a notifier
	// that never signals.
	Notifier inbox.Notifier
	// Hooks gates and observes ingest. Defaults to inbox.NopHooks.
	Hooks inbox.Hooks
	// Verifiers maps verifier names to implementations. Defaults to
	// verify.Default().
	Verifiers *verify.Registry
	// TrustedProxies lists the networks whose X-Forwarded-For header is
	// believed when recording a delivery's source IP. Empty means the header
	// is never trusted.
	TrustedProxies []netip.Prefix
	// Now is the clock used for signature timestamp tolerance. Defaults to
	// time.Now.
	Now func() time.Time
}

// Server is the hookinlet HTTP server.
type Server struct {
	opts    Options
	log     *slog.Logger
	handler http.Handler
	http    *http.Server
}

// New builds a Server. It performs no I/O and makes no network calls.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		return nil, errors.New("server: Options.Addr is required")
	}
	if opts.Store == nil {
		return nil, errors.New("server: Options.Store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}
	if opts.Notifier == nil {
		opts.Notifier = inbox.NopNotifier{}
	}
	if opts.Hooks == nil {
		opts.Hooks = inbox.NopHooks{}
	}
	if opts.Verifiers == nil {
		opts.Verifiers = verify.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	s := &Server{opts: opts, log: opts.Logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// Ingest accepts any method here and answers 405 itself, so that the
	// response is JSON and carries an Allow header.
	mux.HandleFunc("/in/{key}", s.handleIngest)
	mux.HandleFunc("/in/{key}/{rest...}", s.handleIngest)
	mux.HandleFunc("/", notFound)

	// Outermost first. accessLog must see the same *http.Request the mux
	// annotates with r.Pattern, so nothing between it and the mux may clone
	// the request.
	h := requestID(accessLog(opts.Logger, recoverer(opts.Logger, mux)))

	s.handler = h
	s.http = &http.Server{
		Handler: h,
		// No WriteTimeout: SSE streams are long-lived. Handlers that need a
		// deadline set one per request with http.ResponseController.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Handler returns the fully wrapped http.Handler, for embedding or testing.
func (s *Server) Handler() http.Handler { return s.handler }

// Run listens on Options.Addr and serves until ctx is cancelled, then shuts
// down gracefully. It returns nil after a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.opts.Addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve is like Run but accepts an existing listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.Serve(ln) }()
	s.log.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.opts.ShutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
	defer cancel()
	if err := s.Shutdown(sctx); err != nil {
		_ = s.http.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	<-errCh // Serve returns http.ErrServerClosed once Shutdown completes
	return nil
}

// Shutdown stops accepting connections and waits for in-flight requests until
// ctx expires.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether the Store is reachable. Unlike /healthz, which only
// says the process is up, it fails while a dependency is down.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.opts.Store.Ping(ctx); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "not ready", slog.String("err", err.Error()))
		writeError(w, http.StatusServiceUnavailable, "not_ready", "the store is not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the standard error body:
// {"error": {"code": "...", "message": "..."}}.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
