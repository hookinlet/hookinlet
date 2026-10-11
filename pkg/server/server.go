// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package server assembles hookinlet's HTTP surface. Handlers reach storage,
// notification, auth, and secrets only through interfaces in pkg/inbox, so an
// embedder can swap any of them.
//
// Stability: this package is pre-1.0. Its API can still change, and every change is
// listed in CHANGELOG.md; after v1.0.0, semantic versioning applies.
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
	"sync"
	"time"

	"github.com/hookinlet/hookinlet/docs"
	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/specvalidate"
	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
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
	// Authenticator decides who may use the read API (/v1/...). If nil, the read
	// API is not served at all: there is no safe default for who may read.
	// token.NewAuthenticator provides the open-source default.
	Authenticator inbox.Authenticator
	// TrustedProxies lists the networks whose X-Forwarded-For header is
	// believed when recording a delivery's source IP. Empty means the header
	// is never trusted.
	TrustedProxies []netip.Prefix
	// Now is the clock used for signature timestamp tolerance. Defaults to
	// time.Now.
	Now func() time.Time
	// ReadBodyTimeout bounds how long a sender gets to deliver a request body
	// to the ingest endpoint. Defaults to 60s.
	ReadBodyTimeout time.Duration
	// MaxHeaderBytes bounds the request line and headers of any request, which
	// ingest stores verbatim and the read API later returns. Defaults to 64 KiB;
	// Go's own default is 1 MiB. net/http allows about 4 KiB of slack beyond it.
	MaxHeaderBytes int
	// MaxConcurrentReads bounds how many read API requests are served at once.
	// Building a page can take tens of megabytes, so without a bound a few
	// agents polling together could exhaust memory. Defaults to 16.
	MaxConcurrentReads int
	// ReadWaitTimeout is how long a read request waits for a free slot before
	// being told to retry (503). Defaults to 10s.
	ReadWaitTimeout time.Duration
	// MaxStreams bounds how many SSE streams are open at once. Defaults to 1000.
	MaxStreams int
	// MaxStreamsPerInbox bounds how many of them may follow one inbox, so that a
	// single token cannot use every slot. Defaults to half of MaxStreams, and at
	// least 1: one inbox can be followed widely, but never to the exclusion of
	// all the others.
	MaxStreamsPerInbox int
	// StreamRevalidateInterval is how often an open stream is checked against
	// the Authenticator and the Store again. A stream authorized once would
	// otherwise keep running after its token was rotated or its inbox deleted.
	// Defaults to 30s.
	StreamRevalidateInterval time.Duration
	// SSEHeartbeat is how long a stream may be silent before a ping comment is
	// sent. Defaults to 15s.
	SSEHeartbeat time.Duration
	// SSERecheckInterval makes each stream re-read the Store on this interval
	// even without a signal from the Notifier. Zero (the default) turns it off,
	// which is right for the in-process notifier, which never drops a signal. Set
	// it, to a few seconds, when the Notifier can (a distributed one delivers at
	// most once).
	SSERecheckInterval time.Duration
	// DisableMCP turns off the MCP endpoint (/mcp), which is otherwise served
	// whenever an Authenticator is set.
	DisableMCP bool
	// MCPInlineBodyBytes is the largest message body the MCP tools return
	// inline. A larger one is left out and marked body_truncated, and the agent
	// fetches it with get_message. It protects an agent's context window.
	// Defaults to 16 KiB and is limited to 64 KiB.
	MCPInlineBodyBytes int
	// PublicBaseURL is the externally visible address of the server, such as
	// "https://hooks.example.com". The admin API builds ingest URLs from it. If
	// empty, they are built from the request's Host.
	PublicBaseURL string
	// RetentionInterval is how often every inbox's retention policy is applied.
	// Defaults to one hour; a negative value turns the schedule off (the admin
	// API can still run a sweep).
	RetentionInterval time.Duration
	// Version is reported to MCP clients as the server's version. Defaults to "dev".
	Version string
}

// Server is the hookinlet HTTP server.
type Server struct {
	opts    Options
	log     *slog.Logger
	handler http.Handler
	http    *http.Server
	reads   chan struct{} // a slot per read request being served

	spec      *specvalidate.Document // docs/openapi.json, which requests are held to
	mux       *http.ServeMux
	mcpSDK    http.Handler // the MCP SDK's handler, which the mcp operation hands calls to
	adm       *admin.Service
	backupMu  sync.Mutex     // one backup at a time
	retention sync.WaitGroup // the scheduled sweep, so Serve can wait for it

	streams      chan struct{} // a slot per open SSE stream
	streamMu     sync.Mutex
	streamCounts map[inbox.ID]int // open streams per inbox

	// closeCtx is cancelled when the server starts shutting down. Streams watch
	// it, and a write that is blocked on a client that stopped reading is
	// aborted by it, so shutdown never waits on such a client.
	closeCtx    context.Context
	closeCancel context.CancelFunc
	closing     <-chan struct{} // closeCtx.Done()
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
		opts.Notifier = local.New()
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
	if opts.ReadBodyTimeout <= 0 {
		opts.ReadBodyTimeout = 60 * time.Second
	}
	if opts.MaxHeaderBytes <= 0 {
		opts.MaxHeaderBytes = 64 << 10
	}
	if opts.MaxConcurrentReads <= 0 {
		opts.MaxConcurrentReads = 16
	}
	if opts.ReadWaitTimeout <= 0 {
		opts.ReadWaitTimeout = 10 * time.Second
	}
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = 1000
	}
	if opts.MaxStreamsPerInbox <= 0 {
		opts.MaxStreamsPerInbox = max(1, opts.MaxStreams/2)
	}
	if opts.StreamRevalidateInterval <= 0 {
		opts.StreamRevalidateInterval = 30 * time.Second
	}
	if opts.SSEHeartbeat <= 0 {
		opts.SSEHeartbeat = 15 * time.Second
	}
	if opts.RetentionInterval == 0 {
		opts.RetentionInterval = time.Hour
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.MCPInlineBodyBytes <= 0 {
		opts.MCPInlineBodyBytes = 16 << 10
	}
	// Never above what a page query loads for REST: a cap this high would let one
	// call load a hundred times more than a REST page does.
	opts.MCPInlineBodyBytes = min(opts.MCPInlineBodyBytes, wire.InlineBodyBytes)
	if opts.SSERecheckInterval < 0 {
		opts.SSERecheckInterval = 0
	}

	spec, err := specvalidate.Load(docs.OpenAPI)
	if err != nil {
		return nil, err
	}
	closeCtx, closeCancel := context.WithCancel(context.Background())
	s := &Server{
		opts:         opts,
		spec:         spec,
		log:          opts.Logger,
		reads:        make(chan struct{}, opts.MaxConcurrentReads),
		streams:      make(chan struct{}, opts.MaxStreams),
		streamCounts: make(map[inbox.ID]int),
		closeCtx:     closeCtx,
		closeCancel:  closeCancel,
		closing:      closeCtx.Done(),
	}
	s.adm = &admin.Service{Store: opts.Store, Box: opts.SecretBox, Verifiers: opts.Verifiers, Now: opts.Now}
	if err := s.checkMCPTools(); err != nil {
		return nil, err
	}
	s.mcpSDK = s.newMCP()
	mux := http.NewServeMux()
	s.mux = mux
	s.registerRoutes(mux)
	mux.HandleFunc("/", notFound)

	// Outermost first. accessLog must see the same *http.Request the mux
	// annotates with r.Pattern, so nothing between it and the mux may clone
	// the request.
	h := requestID(accessLog(opts.Logger, recoverer(opts.Logger, protective(mux))))

	s.handler = h
	s.http = &http.Server{
		Handler: h,
		// No WriteTimeout: SSE streams are long-lived. Handlers that need a
		// deadline set one per request with http.ResponseController.
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    opts.MaxHeaderBytes,
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

	// Retention starts once the listener is up and never delays it (guardrail 27).
	// Serve does not return until the sweep has stopped, so the caller may close
	// the Store as soon as it does.
	if s.opts.RetentionInterval > 0 {
		s.retention.Add(1)
		go s.retentionLoop()
	}
	defer func() { s.closeCancel(); s.retention.Wait() }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.opts.ShutdownTimeout.String())
	// Tell long-lived streams to finish. This is separate from cancelling request
	// contexts, which would also cut off deliveries that are in the middle of
	// being stored.
	s.closeStreams()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
	defer cancel()
	if err := s.Shutdown(sctx); err != nil {
		_ = s.http.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	<-errCh // Serve returns http.ErrServerClosed once Shutdown completes
	return nil
}

// Shutdown stops accepting connections, ends open streams, and waits for
// in-flight requests until ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeStreams()
	return s.http.Shutdown(ctx)
}

// closeStreams tells every open stream to finish. It is safe to call repeatedly.
func (s *Server) closeStreams() { s.closeCancel() }

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

// retentionLoop applies every inbox's retention policy once, now, and then every
// RetentionInterval until the server shuts down.
func (s *Server) retentionLoop() {
	defer s.retention.Done()
	sweep := func() {
		start := time.Now()
		n, err := s.adm.SweepRetention(s.closeCtx)
		switch {
		case err != nil && s.closeCtx.Err() != nil:
			// Shutting down: the sweep stopped part way, and the next one resumes.
		case err != nil:
			s.log.Warn("retention sweep failed", "deleted", n, "err", err)
		case n > 0:
			s.log.Info("retention sweep", "deleted", n, "took", time.Since(start).Round(time.Millisecond).String())
		}
	}
	sweep()
	t := time.NewTicker(s.opts.RetentionInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			sweep()
		case <-s.closing:
			return
		}
	}
}
