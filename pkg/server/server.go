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
	"sync/atomic"
	"time"

	"github.com/hookinlet/hookinlet/docs"
	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/specvalidate"
	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/notify/local"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// Options configures a Server. Only Store is required (and Addr, to use Run). The
// rule for the rest is one: a zero value means the documented default; a negative
// number turns a bound or a schedule off where that makes sense (RetentionInterval,
// IngestMemoryBudget) and is an error everywhere else; New reports a contradictory or
// out-of-range option (MCPInlineBodyBytes is clamped to its maximum instead), and does no
// I/O.
type Options struct {
	// Addr is the TCP address Run listens on, for example ":8080". It is not needed
	// to Serve on a listener you made, or to mount Handler() in your own http.Server.
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
	// Notifier is told after each newly stored message. Defaults to the
	// in-process notifier (pkg/notify/local), which signals streams and waits at once.
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
	// DrainDelay is how long the server goes on serving, but reports itself not
	// ready (/readyz answers 503), after it is told to stop and before it stops
	// listening. A load balancer that polls readiness takes the instance out of
	// rotation in that time, so that no request is sent to a listener that is
	// closing. Zero (the default) stops at once. It comes out of the time before
	// ShutdownTimeout begins, not out of it.
	DrainDelay time.Duration
	// IngestMemoryBudget bounds the bytes of request bodies held in memory at once by
	// deliveries being received. The bytes are counted as they arrive and held until
	// the delivery is stored or refused, so a connection that has promised a body and
	// not sent it costs nothing; a delivery whose bytes would take the total over is
	// answered 503 with Retry-After. The memory used is a small multiple of the bytes
	// counted. Defaults to 256 MiB, must be at least the largest body an inbox may take
	// (10 MiB), and a negative value turns the bound off.
	IngestMemoryBudget int64
	// ResponseTimeout bounds how long a response may take to be written to the client,
	// from the moment the request has been checked. A client that sends a request and
	// does not read the answer would otherwise keep the answer (a page of messages, a
	// raw body) in memory for as long as it likes. It is a fixed budget for the whole
	// answer from when the server starts building it, so a client must read at roughly
	// 16 MiB per ResponseTimeout to take the largest page. Ingest, the admin API, streams
	// and backups are not bound by it (streams and backups manage their own deadlines).
	// Defaults to 2 minutes.
	ResponseTimeout time.Duration
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
	// otherwise keep running after its token was revoked or its inbox deleted.
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
	// DisableAdmin turns off the admin API altogether: its routes answer 404 and
	// its routes are not mounted. (The admin token still reads inboxes through the read API and MCP, as it always does.) Inboxes and tokens are then managed with
	// the command line, which works in the data directory.
	DisableAdmin bool
	// DisableUI turns off the admin console (the web app served at /admin on the same
	// listener as the admin API). It is also absent when the admin API is, or has no
	// Authenticator to guard it.
	DisableUI bool
	// AdminAddr, if set, serves the admin API on its own listener at this address
	// and not on Addr. That lets the admin surface sit on a loopback or private
	// interface while ingest and the read API are public. Empty serves everything
	// on Addr. Run listens on it; with listeners of your own, use ServeAdmin (Serve alone
	// would leave the admin API unreachable, so it refuses).
	AdminAddr string
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

	adminHandler http.Handler // set when AdminAddr is
	adminHTTP    *http.Server
	reads        chan struct{} // a slot per read request being served

	metrics      metrics
	draining     atomic.Bool // set when the server is told to stop: /readyz fails
	ingestBudget byteBudget  // body bytes held by deliveries being received

	spec       *specvalidate.Document // docs/openapi.json, which requests are held to
	mux        *http.ServeMux
	mcpSDK     http.Handler // the MCP SDK's handler, which the mcp operation hands calls to
	adm        *admin.Service
	secretsMu  sync.Mutex // guards the next three: the last check of the stored secrets
	secretsAt  time.Time
	secretsErr error
	backupMu   sync.Mutex     // one backup at a time
	retained   retentionRun   // the last retention run that finished
	retention  sync.WaitGroup // the scheduled sweep, so Serve can wait for it

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
	if opts.Store == nil {
		return nil, errors.New("server: Options.Store is required")
	}
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"ShutdownTimeout", int64(opts.ShutdownTimeout)}, {"ResponseTimeout", int64(opts.ResponseTimeout)}, {"ReadBodyTimeout", int64(opts.ReadBodyTimeout)},
		{"MaxHeaderBytes", int64(opts.MaxHeaderBytes)}, {"MaxConcurrentReads", int64(opts.MaxConcurrentReads)},
		{"ReadWaitTimeout", int64(opts.ReadWaitTimeout)}, {"MaxStreams", int64(opts.MaxStreams)},
		{"MaxStreamsPerInbox", int64(opts.MaxStreamsPerInbox)}, {"StreamRevalidateInterval", int64(opts.StreamRevalidateInterval)},
		{"SSEHeartbeat", int64(opts.SSEHeartbeat)}, {"SSERecheckInterval", int64(opts.SSERecheckInterval)},
		{"MCPInlineBodyBytes", int64(opts.MCPInlineBodyBytes)}, {"DrainDelay", int64(opts.DrainDelay)},
	} {
		if f.v < 0 {
			return nil, fmt.Errorf("server: Options.%s must not be negative (zero means the default)", f.name)
		}
	}
	if opts.AdminAddr != "" && opts.DisableAdmin {
		return nil, errors.New("server: Options.AdminAddr and Options.DisableAdmin contradict each other")
	}
	if opts.AdminAddr != "" && opts.AdminAddr == opts.Addr {
		return nil, errors.New("server: Options.AdminAddr is the same as Options.Addr; leave it empty to serve both there")
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
	realClock := opts.Now == nil
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IngestMemoryBudget == 0 {
		opts.IngestMemoryBudget = 256 << 20
	}
	if opts.IngestMemoryBudget > 0 && opts.IngestMemoryBudget < inbox.MaxBodyBytesCeiling {
		return nil, fmt.Errorf("server: Options.IngestMemoryBudget must be at least %d bytes, the largest body an inbox may take, or a delivery that is allowed could never be received", inbox.MaxBodyBytesCeiling)
	}
	if opts.ResponseTimeout == 0 {
		opts.ResponseTimeout = 2 * time.Minute
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
		ingestBudget: byteBudget{limit: opts.IngestMemoryBudget},
		streamCounts: make(map[inbox.ID]int),
		closeCtx:     closeCtx,
		closeCancel:  closeCancel,
		closing:      closeCtx.Done(),
	}
	s.adm = &admin.Service{Store: opts.Store, Box: opts.SecretBox, Verifiers: opts.Verifiers, Now: opts.Now, Log: opts.Logger, GuardClock: realClock}
	if err := s.checkMCPTools(); err != nil {
		return nil, err
	}
	s.mcpSDK = s.newMCP()
	surface := surfaceAll
	if opts.AdminAddr != "" {
		surface = surfacePublic
	}
	s.handler = s.build(surface)
	s.http = s.newHTTP(s.handler)
	if opts.AdminAddr != "" {
		s.adminHandler = s.build(surfaceAdmin)
		s.adminHTTP = s.newHTTP(s.adminHandler)
	}
	return s, nil
}

// build mounts the routes of a surface and wraps them in the middleware.
func (s *Server) build(surface surface) http.Handler {
	mux := http.NewServeMux()
	if surface != surfaceAdmin {
		s.mux = mux
	}
	s.registerRoutes(mux, surface)
	s.mountConsole(mux, surface)
	mux.HandleFunc("/", notFound)
	// Outermost first. accessLog must see the same *http.Request the mux
	// annotates with r.Pattern, so nothing between it and the mux may clone
	// the request.
	return requestID(accessLog(s.log, recoverer(s.log, s.counting(protective(mux)))))
}

func (s *Server) newHTTP(h http.Handler) *http.Server {
	return &http.Server{
		Handler: h,
		// No WriteTimeout: SSE streams are long-lived. Handlers that need a
		// deadline set one per request with http.ResponseController.
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    s.opts.MaxHeaderBytes,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}

// Handler returns the fully wrapped http.Handler, for embedding or testing. When
// Options.AdminAddr is set it does not serve the admin API; AdminHandler does.
func (s *Server) Handler() http.Handler { return s.handler }

// AdminHandler returns the handler for the admin API when it has its own
// listener (Options.AdminAddr), and nil otherwise.
func (s *Server) AdminHandler() http.Handler { return s.adminHandler }

// Run listens on Options.Addr and serves until ctx is cancelled, then shuts
// down gracefully. It returns nil after a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	if s.opts.Addr == "" {
		return errors.New("server: Options.Addr is required to Run; to serve on a listener you made, use Serve")
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.opts.Addr, err)
	}
	if s.opts.AdminAddr == "" {
		return s.Serve(ctx, ln)
	}
	adminLn, err := lc.Listen(ctx, "tcp", s.opts.AdminAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("listen %s: %w", s.opts.AdminAddr, err)
	}
	return s.ServeAdmin(ctx, ln, adminLn)
}

// Serve is like Run but accepts an existing listener. It refuses a server with
// Options.AdminAddr set (the admin API is then on a second listener: use ServeAdmin or
// Run). To put the admin API on a server of your own, mount AdminHandler beside Handler.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		return errors.New("server: Serve needs a listener")
	}
	if s.adminHTTP != nil {
		_ = ln.Close()
		return errors.New("server: Options.AdminAddr is set, so the admin API is on its own listener; use ServeAdmin with a listener for each, or Run")
	}
	return s.serve(ctx, ln, nil)
}

// ServeAdmin is Serve for a server with Options.AdminAddr set, given a listener
// for each address. The admin API, and the console if it is on, are served on adminLn and nothing else is.
func (s *Server) ServeAdmin(ctx context.Context, ln, adminLn net.Listener) error {
	if ln == nil || adminLn == nil {
		if ln != nil {
			_ = ln.Close()
		}
		if adminLn != nil {
			_ = adminLn.Close()
		}
		return errors.New("server: ServeAdmin needs a listener for each address")
	}
	if s.adminHTTP == nil {
		_ = ln.Close()
		_ = adminLn.Close()
		return errors.New("server: ServeAdmin needs Options.AdminAddr")
	}
	return s.serve(ctx, ln, adminLn)
}

func (s *Server) serve(ctx context.Context, ln, adminLn net.Listener) error {
	if s.closeCtx.Err() != nil {
		_ = ln.Close()
		if adminLn != nil {
			_ = adminLn.Close()
		}
		return errors.New("server: a Server cannot be started again once it has stopped; make a new one")
	}
	errCh := make(chan error, 2)
	go func() { errCh <- s.http.Serve(ln) }()
	s.log.Info("listening", "addr", ln.Addr().String())
	servers := 1
	if adminLn != nil {
		go func() { errCh <- s.adminHTTP.Serve(adminLn) }()
		s.log.Info("listening for the admin API", "addr", adminLn.Addr().String())
		servers++
	}

	// Retention starts once the listener is up and never delays it (startup makes no network calls and reaches listening fast).
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
		// One listener failing takes the other down with it.
		_ = s.http.Close()
		if s.adminHTTP != nil {
			_ = s.adminHTTP.Close()
		}
		return err
	case <-ctx.Done():
	}

	pending := servers // listeners that have not yet reported on errCh
	if d := s.opts.DrainDelay; d > 0 {
		s.draining.Store(true)
		s.log.Info("draining: reporting not ready before closing", "delay", d.String())
		select {
		case <-time.After(d):
		case err := <-errCh:
			// A listener ended meanwhile: failed, or closed by an embedder's call to
			// Shutdown. Either way stop waiting out the delay.
			pending--
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				_ = s.http.Close()
				if s.adminHTTP != nil {
					_ = s.adminHTTP.Close()
				}
				return err
			}
		}
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
		if s.adminHTTP != nil {
			_ = s.adminHTTP.Close()
		}
		return fmt.Errorf("shutdown: %w", err)
	}
	for ; pending > 0; pending-- {
		<-errCh // each returns http.ErrServerClosed once Shutdown completes
	}
	return nil
}

// Shutdown stops accepting connections, ends open streams, and waits for
// in-flight requests until ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeStreams()
	err := s.http.Shutdown(ctx)
	if s.adminHTTP != nil {
		err = errors.Join(err, s.adminHTTP.Shutdown(ctx))
	}
	return err
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
	s.sweepLoop(s.closeCtx)
}

// Maintain runs the scheduled work (the retention sweep, and the finishing of any
// delete that was interrupted) until ctx is done or the server is shut down. Serve
// and Run do this themselves; call Maintain only when you serve Handler() from an
// http.Server of your own, or the sweep never runs. It does nothing if the schedule
// is off (a negative RetentionInterval). It returns when it stops.
func (s *Server) Maintain(ctx context.Context) {
	if s.opts.RetentionInterval < 0 || s.closeCtx.Err() != nil || ctx.Err() != nil {
		return // nothing to do, or the server is already shut down and its Store may be closing
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	s.sweepLoop(ctx)
}

// retentionRun remembers the last retention run that finished without an error.
type retentionRun struct {
	mu      sync.Mutex
	at      time.Time
	deleted int64
}

func (r *retentionRun) record(at time.Time, deleted int64, err error) {
	if err != nil {
		return
	}
	r.mu.Lock()
	r.at, r.deleted = at, deleted
	r.mu.Unlock()
}

func (r *retentionRun) last() (at time.Time, deleted int64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.at, r.deleted, !r.at.IsZero()
}

func (s *Server) sweepLoop(ctx context.Context) {
	sweep := func() {
		start := time.Now()
		n, err := s.adm.SweepRetention(ctx)
		s.retained.record(s.opts.Now(), n, err)
		s.metrics.sweeps.Add(1)
		s.metrics.retention.Add(n)
		switch {
		case err != nil && ctx.Err() != nil:
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
		case <-ctx.Done():
			return
		}
	}
}
