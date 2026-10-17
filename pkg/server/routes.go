// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hookinlet/hookinlet/internal/api"
	"github.com/hookinlet/hookinlet/internal/specvalidate"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// This file is where the document becomes a server. Which routes exist, who may
// call each, and what a request may carry are read from docs/openapi.json; nothing
// here names a route.
//
//   - The routes are registered by the generated code, once for every operation of
//     the document (api.HandlerWithOptions). docRouter only gives the multi-segment
//     path its net/http spelling and leaves out what this server is not
//     configured to serve.
//   - enforce runs before every operation: the credential the operation's
//     `security` asks for, the path parameters, the query string, and the JSON body,
//     in that order. An anonymous caller learns nothing, because the credential is
//     checked before anything else is looked at.
//   - A method a path does not take is answered by a handler made from the same
//     list of operations (405 and an Allow header that names the methods the
//     document gives the path).
//
// The only route written in Go is the catch-all that answers 404.

// apiServer is every operation of the document. It embeds one type per family and
// no more: the compiler refuses it if an operation has no implementation, or has
// two.
type apiServer struct {
	*adminAPI
	*readAPI
	*healthAPI
	*ingestAPI
	*streamAPI
	*mcpAPI
}

func (s *Server) newAPI() *apiServer {
	return &apiServer{
		adminAPI:  &adminAPI{s: s},
		readAPI:   &readAPI{s: s},
		healthAPI: &healthAPI{s: s},
		ingestAPI: &ingestAPI{s: s},
		streamAPI: &streamAPI{s: s},
		mcpAPI:    &mcpAPI{s: s},
	}
}

// registerRoutes mounts every operation of the document the server is configured
// to serve.
func (s *Server) registerRoutes(mux *http.ServeMux, surface surface) {
	rt := &docRouter{s: s, mux: mux, surface: surface, served: map[string][]string{}}
	api.HandlerWithOptions(s.strictServer(s.newAPI()), api.StdHTTPServerOptions{
		BaseRouter:       rt,
		Middlewares:      []api.MiddlewareFunc{s.enforce},
		ErrorHandlerFunc: s.paramError,
	})
	rt.methodFallbacks()
}

// serves reports whether this server is set up to serve an operation: those that
// need a credential exist only when there is an Authenticator to say who may call
// (there is no safe default for who may read), and MCP can be turned off.
func (s *Server) serves(op *specvalidate.Operation, surface surface) bool {
	admin := op.HasTag("Admin")
	switch {
	case op.Security() != specvalidate.Open && s.opts.Authenticator == nil:
		return false
	case op.HasTag("MCP") && s.opts.DisableMCP:
		return false
	case admin && s.opts.DisableAdmin:
		return false
	case admin && surface == surfacePublic:
		return false
	case !admin && surface == surfaceAdmin && (!op.HasTag("Read") || s.opts.DisableUI):
		// The admin listener serves the admin API, and, for the console it also serves, the read
		// API the console shows messages with (it is the same authentication as anywhere else).
		return false
	}
	return true
}

// surface is which operations a listener serves.
type surface int

const (
	surfaceAll    surface = iota // everything: one listener
	surfacePublic                // all but the admin API
	surfaceAdmin                 // the admin API only
)

// docRouter is the router the generated code registers its routes on.
type docRouter struct {
	s       *Server
	mux     *http.ServeMux
	surface surface
	served  map[string][]string // path template -> methods registered
}

func (d *docRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) { d.mux.ServeHTTP(w, r) }

func (d *docRouter) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	method, path, _ := strings.Cut(pattern, " ")
	op := d.s.spec.Lookup(method, path)
	if op == nil || !d.s.serves(op, d.surface) {
		return
	}
	d.mux.HandleFunc(method+" "+op.Pattern(), h)
	d.served[op.Path] = append(d.served[op.Path], method)
}

// methodFallbacks answers every other method on each path that is served. A path
// with a GET operation and no HEAD one refuses HEAD too: net/http would otherwise
// answer it from the GET handler.
func (d *docRouter) methodFallbacks() {
	for _, route := range d.s.spec.Routes() {
		methods := d.served[route.Path]
		if len(methods) == 0 {
			continue
		}
		sort.Strings(methods)
		h := d.s.methodNotAllowed(route.Security, methods)
		d.mux.HandleFunc(route.Pattern, h)
		if has(methods, http.MethodGet) && !has(methods, http.MethodHead) {
			d.mux.HandleFunc(http.MethodHead+" "+route.Pattern, h)
		}
	}
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// methodNotAllowed answers a method the path does not take. Where the path needs
// a credential it is checked first, so that an anonymous caller learns nothing
// about the API.
func (s *Server) methodNotAllowed(sec specvalidate.Security, methods []string) http.HandlerFunc {
	allow := strings.Join(methods, ", ")
	said := allow
	if len(methods) == 2 {
		said = methods[0] + " and " + methods[1]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if sec != specvalidate.Open {
			if _, ok := s.authenticate(w, r, sec == specvalidate.Admin); !ok {
				return
			}
		}
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint accepts "+said)
	}
}

// readRequest is what an operation needs besides what the generated interface
// passes it: the request, the response writer (a HEAD sets the headers a GET would
// have, a download clears its write deadline), who is calling, and the inbox the
// caller was allowed to read.
type readRequest struct {
	w         http.ResponseWriter
	r         *http.Request
	in        *inbox.Inbox
	principal inbox.Principal
	// hold is the read slot a handler handed over to be released once the response has
	// been written, not when the handler returns: the answer (a page, a raw body) sits in
	// memory until the client has taken it, and the slots are what bound how much.
	hold func()
	// deadline starts the response timeout. It is started when the answer begins to
	// be built, not when the request arrives: ingest and admin calls may run long
	// before they answer, and the time a handler spends is not the client's.
	deadline func()
}

const (
	requestKey ctxKey = 1
	readKey    ctxKey = 2
	writerKey  ctxKey = 3
)

func readFrom(ctx context.Context) *readRequest { return ctx.Value(readKey).(*readRequest) }

// scopedTo returns ctx scoped to the principal's tenant if it has one (see
// inbox.TenantPrincipal), and ctx itself if not. Everything the server does for a
// caller with the Store goes through a context made this way, so that the names a
// caller uses are looked up in its tenant and nothing outside it is reachable.
func scopedTo(ctx context.Context, p inbox.Principal) context.Context {
	if tp, ok := p.(inbox.TenantPrincipal); ok {
		return inbox.WithTenant(ctx, tp.Tenant())
	}
	return ctx
}

// context gives the generated handlers, which are passed a context and nothing
// else, what enforce found out.
func (rr *readRequest) context(ctx context.Context) context.Context {
	ctx = scopedTo(ctx, rr.principal)
	ctx = context.WithValue(ctx, requestKey, rr.r)
	ctx = context.WithValue(ctx, writerKey, rr.w)
	return context.WithValue(ctx, readKey, rr)
}

// authenticate checks the credential and, for the admin API, that it is the
// admin's. It reports false after answering.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, adminOnly bool) (inbox.Principal, bool) {
	principal, err := s.opts.Authenticator.Authenticate(r)
	switch {
	case errors.Is(err, inbox.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="hookinlet"`)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return nil, false
	case err != nil:
		s.internalError(r.Context(), w, "authenticate", err)
		return nil, false
	case s.refusesUntenanted(r.Context(), principal):
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return nil, false
	case adminOnly && !principal.CanAdmin():
		writeError(w, http.StatusForbidden, "forbidden", "this credential cannot use the admin API")
		return nil, false
	}
	return principal, true
}

// refusesUntenanted reports whether Options.RequireTenant refuses p: a principal that is
// in no tenant and is not an admin. It logs why when it does. The caller is told only
// that something went wrong on the server, which is the truth: the Authenticator gave a
// principal the embedder said it never would.
func (s *Server) refusesUntenanted(ctx context.Context, p inbox.Principal) bool {
	if !s.opts.RequireTenant {
		return false
	}
	if _, ok := p.(inbox.TenantPrincipal); ok || (p != nil && p.CanAdmin()) {
		return false
	}
	s.log.LogAttrs(ctx, slog.LevelError,
		"refused a caller in no tenant: Options.RequireTenant is set, and the Authenticator returned a principal that is not an inbox.TenantPrincipal and cannot admin",
		slog.String("principal", fmt.Sprintf("%T", p)))
	return true
}

// secure does what the operation's `security` asks. An operation that needs no
// credential asks nothing. An agent operation on an inbox finds the inbox only if
// the caller may read it: an inbox that is not there and one the credential may
// not see are the same 404, so one inbox's credential cannot be used to learn
// whether another exists.
func (s *Server) secure(w http.ResponseWriter, r *http.Request, op *specvalidate.Operation, rr *readRequest) bool {
	sec := op.Security()
	if sec == specvalidate.Open {
		return true
	}
	principal, ok := s.authenticate(w, r, sec == specvalidate.Admin)
	if !ok {
		return false
	}
	rr.principal = principal
	name, named := op.InboxParam()
	if sec != specvalidate.Agent || !named {
		return true
	}
	in, err := s.opts.Store.GetInboxBySlug(scopedTo(r.Context(), principal), r.PathValue(name))
	switch {
	case errors.Is(err, inbox.ErrNotFound), err == nil && !principal.CanReadInbox(in):
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return false
	case err != nil:
		s.internalError(r.Context(), w, "load inbox", err)
		return false
	}
	rr.in = in
	return true
}

// enforce runs before every operation.
func (s *Server) enforce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := s.operation(r)
		if op == nil {
			// A route the document does not describe would be served with none of the
			// checks. That is a mistake in the code, so it is refused loudly.
			s.log.LogAttrs(r.Context(), slog.LevelError, "a route is not in the document",
				slog.String("route", r.Pattern), slog.String("method", r.Method))
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		rr := &readRequest{w: w, r: r}
		if !s.secure(w, r, op, rr) || !s.checkRequest(w, r, op) {
			return
		}
		// A client that asks and does not read must not keep the answer in memory for
		// ever (see holdSlot). Operations that run long on purpose set their own deadlines.
		rr.deadline = func() {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.opts.ResponseTimeout))
		}
		// The deadline every request starts with (see deadlines) counts from its arrival.
		// One without a credential keeps it: a health check answers at once, and an
		// ingest starts its answer's deadline again when it is ready. An agent operation
		// may wait before it answers (for a free read slot, or in an MCP wait), so it
		// gets that time as well; the operations that wait bound their answer from when
		// it is ready (rr.deadline), and a stream or a backup sets its own deadline for
		// every write. Everything else it answers early, a refusal or an error, must be
		// taken within that time, or a client with a token could pipeline such requests
		// and never read the answers. An admin operation may run for as long as the work
		// takes (a large listing), and its caller holds the admin token, so it has none.
		switch op.Security() {
		case specvalidate.Admin:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		case specvalidate.Agent:
			allowance := s.opts.ResponseTimeout + s.opts.ReadWaitTimeout
			if strings.HasSuffix(r.Pattern, " /mcp") {
				allowance += mcpMaxWait * time.Second // a wait_for_messages call
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(allowance))
		}
		// However the handler ends, even by aborting the connection, the slot goes back.
		defer func() {
			if rr.hold != nil {
				rr.hold()
			}
		}()
		// The hooks are told which operation this is and the id the access log has for
		// it (inbox.AccessFrom). An MCP tool call names its tool instead (mcpTools.bind).
		ctx := inbox.WithAccess(r.Context(), inbox.AccessInfo{Operation: op.ID, RequestID: RequestID(r.Context())})
		next.ServeHTTP(w, r.WithContext(rr.context(ctx)))
	})
}

// checkRequest holds the request to the document: its path parameters, query
// string, and JSON body. It reports false after answering.
func (s *Server) checkRequest(w http.ResponseWriter, r *http.Request, op *specvalidate.Operation) bool {
	// A path parameter outside its schema names something that cannot exist.
	if len(op.CheckPath(r.PathValue)) > 0 {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return false
	}
	// An operation that keeps the query string as the sender wrote it takes whatever
	// it is, malformed or not: a delivery is not refused for its URL.
	if !op.AnyQuery() {
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_parameter", "the query string is malformed")
			return false
		}
		if vs := op.CheckQuery(values); len(vs) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_parameter", parameterMessage(vs))
			return false
		}
	}
	limit := op.MaxBodyBytes()
	if !op.TakesBody() || limit == 0 {
		return true
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return false
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "the request body could not be read")
		return false
	}
	if vs := op.CheckBody(body); len(vs) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "the request body is not valid: "+clip(vs[0].String(), 200))
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}

// paramError answers a path or query parameter the generated code could not read.
// The credential is still checked first. A seq that is not a number is a message
// that does not exist.
func (s *Server) paramError(w http.ResponseWriter, r *http.Request, err error) {
	if op := s.operation(r); op != nil {
		if !s.secure(w, r, op, &readRequest{}) {
			return
		}
		// The document's own check says what is wrong, and says it the same way
		// everywhere. If it finds nothing, the generated code was stricter.
		if !s.checkRequest(w, r, op) {
			return
		}
	}
	var bad *api.InvalidParamFormatError
	if errors.As(err, &bad) && bad.ParamName == "seq" {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	var many *api.TooManyValuesForParamError
	var format *api.InvalidParamFormatError
	switch {
	case errors.As(err, &many):
		writeError(w, http.StatusBadRequest, "invalid_parameter", `parameter "`+truncateName(many.ParamName)+`": may be given only once`)
	case errors.As(err, &format):
		writeError(w, http.StatusBadRequest, "invalid_parameter", `parameter "`+truncateName(format.ParamName)+`": is not valid`)
	default:
		writeError(w, http.StatusBadRequest, "invalid_parameter", "a path or query parameter is not valid")
	}
}
