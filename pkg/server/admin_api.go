// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hookinlet/hookinlet/internal/admin"
	"github.com/hookinlet/hookinlet/internal/api"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// adminAPI serves the admin operations that return JSON. It implements the
// methods of api.StrictServerInterface for them, so each one can only answer
// with a response the document lists for that operation: returning a status the
// document does not describe does not compile.
//
// The checks that come before an operation (the caller's credential, the method,
// the query string, the body's size, type, and shape) are made by admin() and
// adminBody, and are covered by the contract tests, which force each of those
// responses and check it against the document.
type adminAPI struct {
	s *Server
}

// fail is a failed operation, classified. status is the HTTP status the error
// belongs to; an operation whose document does not list that status answers 500
// instead of an undocumented one.
type fail struct {
	status int
	code   string
	msg    string
	op     string // set by classify
}

func (s *Server) classify(ctx context.Context, op string, err error) fail {
	f := s.classifyErr(ctx, op, err)
	f.op = op
	return f
}

func (s *Server) classifyErr(ctx context.Context, op string, err error) fail {
	switch {
	case errors.Is(err, inbox.ErrInvalid):
		return fail{http.StatusBadRequest, "invalid_parameter", strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "), ""}
	case errors.Is(err, inbox.ErrNotFound):
		return fail{http.StatusNotFound, "not_found", "not found", ""}
	case errors.Is(err, inbox.ErrConflict) && strings.Contains(err.Error(), "ingest key"):
		return fail{http.StatusConflict, "conflict", "that ingest key is already in use by another inbox", ""}
	case errors.Is(err, inbox.ErrConflict):
		return fail{http.StatusConflict, "conflict", "an inbox with that slug already exists, or it was changed at the same time; check and retry", ""}
	case errors.Is(err, admin.ErrBusy):
		return fail{http.StatusConflict, "busy", "that is already running", ""}
	case errors.Is(err, admin.ErrNoSecretBox), errors.Is(err, admin.ErrNoBackup):
		return fail{http.StatusNotImplemented, "not_implemented", err.Error(), ""}
	}
	level := slog.LevelError
	if errors.Is(err, context.Canceled) {
		level = slog.LevelDebug // the client went away; that is not a fault on our side
	}
	s.log.LogAttrs(ctx, level, "request failed", slog.String("op", op), slog.String("err", err.Error()))
	return fail{http.StatusInternalServerError, "internal", "internal error", ""}
}

func requestIDOf(ctx context.Context) string { return RequestID(ctx) }

func badRequest(ctx context.Context, f fail) api.BadRequestJSONResponse {
	var b api.ErrorBadRequest
	b.Error.Code, b.Error.Message = api.ErrorBadRequestErrorCode(f.code), f.msg
	return api.BadRequestJSONResponse{Body: b, Headers: api.BadRequestResponseHeaders{XRequestId: requestIDOf(ctx)}}
}

func notFoundResp(ctx context.Context, f fail) api.NotFoundJSONResponse {
	var b api.ErrorNotFound
	b.Error.Code, b.Error.Message = api.ErrorNotFoundErrorCode(f.code), f.msg
	return api.NotFoundJSONResponse{Body: b, Headers: api.NotFoundResponseHeaders{XRequestId: requestIDOf(ctx)}}
}

func conflict(ctx context.Context, f fail) api.ConflictJSONResponse {
	var b api.ErrorConflict
	b.Error.Code, b.Error.Message = api.ErrorConflictErrorCode(f.code), f.msg
	return api.ConflictJSONResponse{Body: b, Headers: api.ConflictResponseHeaders{XRequestId: requestIDOf(ctx)}}
}

func notImplemented(ctx context.Context, f fail) api.NotImplementedJSONResponse {
	var b api.ErrorNotImplemented
	b.Error.Code, b.Error.Message = api.ErrorNotImplementedErrorCode(f.code), f.msg
	return api.NotImplementedJSONResponse{Body: b, Headers: api.NotImplementedResponseHeaders{XRequestId: requestIDOf(ctx)}}
}

// fell answers 500 for a failure the operation's document has no status for, and
// says so in the log: a Store (or anything behind it) failed in a way the caller
// would have been told about on another operation.
func (s *Server) fell(ctx context.Context, f fail) api.InternalJSONResponse {
	if f.status != http.StatusInternalServerError {
		s.log.LogAttrs(ctx, slog.LevelError, "request failed", slog.String("op", f.op),
			slog.String("err", "the failure is a "+strconv.Itoa(f.status)+" ("+f.code+"), which the document does not list for this operation"))
	}
	return internal(ctx)
}

func internal(ctx context.Context) api.InternalJSONResponse {
	var b api.ErrorInternal
	b.Error.Code, b.Error.Message = api.ErrorInternalErrorCodeInternal, "internal error"
	return api.InternalJSONResponse{Body: b, Headers: api.InternalResponseHeaders{XRequestId: requestIDOf(ctx)}}
}

// view converts an inbox as the admin service describes it to the document's
// type, so a field the document does not have cannot be sent by accident.
func view(v admin.View, base string) api.Admin {
	v = v.WithBase(base)
	cfg := map[string]any{}
	if len(v.VerifierConfig) > 0 {
		// Numbers stay as written: json.Number encodes as its own text.
		dec := json.NewDecoder(bytes.NewReader(v.VerifierConfig))
		dec.UseNumber()
		_ = dec.Decode(&cfg)
	}
	out := api.Admin{
		CreatedAt: v.CreatedAt, Disabled: v.Disabled, EventTypePointer: v.EventTypePointer, IngestKey: v.IngestKey,
		HasVerifierSecret: v.HasVerifierSecret, Id: int(v.ID), IngestPath: v.IngestPath,
		LastSeq: int(v.LastSeq), MaxBodyBytes: int(v.MaxBodyBytes), Name: v.Name,
		OnVerifyFail: api.AdminOnVerifyFail(v.OnVerifyFail), RetentionDays: v.RetentionDays,
		RetentionMaxCount: int(v.RetentionMaxCount), Slug: v.Slug, UpdatedAt: v.UpdatedAt,
		Verifier: v.Verifier, VerifierConfig: cfg, Version: int(v.Version),
	}
	if v.IngestURL != "" {
		out.IngestUrl = &v.IngestURL
	}
	if len(v.Warnings) > 0 {
		w := v.Warnings
		out.Warnings = &w
	}
	if v.MessageCount != nil {
		n := int(*v.MessageCount)
		out.MessageCount = &n
	}
	return out
}

func created(c *admin.Created, base string) api.Created {
	out := api.Created{AgentToken: c.AgentToken, Inbox: view(c.Inbox, base)}
	if len(c.Warnings) > 0 {
		out.Warnings = &c.Warnings
	}
	return out
}

func tokenView(v admin.TokenView) api.Token {
	at, _ := time.Parse(admin.TimeLayout, v.CreatedAt)
	return api.Token{AllInboxes: v.AllInboxes, CreatedAt: at, Id: int(v.ID), Inboxes: v.Inboxes, Name: v.Name}
}

func rawConfig(m *map[string]any) json.RawMessage {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(*m)
	return b
}

func orZero[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func (a *adminAPI) CreateInbox(ctx context.Context, req api.CreateInboxRequestObject) (api.CreateInboxResponseObject, error) {
	b := req.Body
	p := admin.CreateParams{
		Slug: b.Slug, IngestKey: orZero(b.IngestKey), Name: orZero(b.Name), Verifier: b.Verifier, VerifierConfig: rawConfig(b.VerifierConfig),
		VerifierSecret: orZero(b.VerifierSecret), MaxBodyBytes: int64(orZero(b.MaxBodyBytes)),
		RetentionDays: orZero(b.RetentionDays), RetentionMaxCount: int64(orZero(b.RetentionMaxCount)),
		EventTypePointer: orZero(b.EventTypePointer),
	}
	if b.OnVerifyFail != nil {
		p.OnVerifyFail = string(*b.OnVerifyFail)
	}
	c, err := a.s.adm.Create(ctx, p)
	if err != nil {
		f := a.s.classify(ctx, "create inbox", err)
		switch f.status {
		case http.StatusBadRequest:
			return api.CreateInbox400JSONResponse{BadRequestJSONResponse: badRequest(ctx, f)}, nil
		case http.StatusConflict:
			return api.CreateInbox409JSONResponse{ConflictJSONResponse: conflict(ctx, f)}, nil
		case http.StatusNotImplemented:
			return api.CreateInbox501JSONResponse{NotImplementedJSONResponse: notImplemented(ctx, f)}, nil
		}
		return api.CreateInbox500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	base := a.s.baseURLFor(ctx)
	a.s.auditCtx(ctx, "create inbox", c.Inbox.Slug)
	return api.CreateInbox201JSONResponse{
		Body:    created(c, base),
		Headers: api.CreateInbox201ResponseHeaders{Location: "/admin/inboxes/" + c.Inbox.Slug, XRequestId: requestIDOf(ctx)},
	}, nil
}

func (a *adminAPI) ListInboxes(ctx context.Context, req api.ListInboxesRequestObject) (api.ListInboxesResponseObject, error) {
	limit := orZero(req.Params.Limit)
	views, next, err := a.s.adm.List(ctx, inbox.ID(orZero(req.Params.After)), limit)
	if err != nil {
		f := a.s.classify(ctx, "list inboxes", err)
		if f.status == http.StatusBadRequest {
			return api.ListInboxes400JSONResponse{BadRequestJSONResponse: badRequest(ctx, f)}, nil
		}
		return api.ListInboxes500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	base := a.s.baseURLFor(ctx)
	out := api.AdminList{Inboxes: make([]api.Admin, len(views))}
	for i, v := range views {
		out.Inboxes[i] = view(v, base)
	}
	if next != 0 {
		n := "/admin/inboxes?after=" + strconv.FormatInt(int64(next), 10)
		if limit > 0 {
			n += "&limit=" + strconv.Itoa(limit)
		}
		out.Next = &n
	}
	return api.ListInboxes200JSONResponse{Body: out, Headers: api.ListInboxes200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *adminAPI) ShowInbox(ctx context.Context, req api.ShowInboxRequestObject) (api.ShowInboxResponseObject, error) {
	v, err := a.s.adm.Get(ctx, req.Slug)
	if err != nil {
		f := a.s.classify(ctx, "show inbox", err)
		if f.status == http.StatusNotFound {
			return api.ShowInbox404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		}
		return api.ShowInbox500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	return api.ShowInbox200JSONResponse{Body: view(v, a.s.baseURLFor(ctx)), Headers: api.ShowInbox200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *adminAPI) UpdateInbox(ctx context.Context, req api.UpdateInboxRequestObject) (api.UpdateInboxResponseObject, error) {
	b := req.Body
	p := admin.Patch{
		Name: b.Name, Verifier: b.Verifier, EventTypePointer: b.EventTypePointer, Disabled: b.Disabled,
		RetentionDays: b.RetentionDays,
	}
	if b.VerifierConfig != nil {
		raw := rawConfig(b.VerifierConfig)
		p.VerifierConfig = &raw
	}
	if b.OnVerifyFail != nil {
		s := string(*b.OnVerifyFail)
		p.OnVerifyFail = &s
	}
	if b.MaxBodyBytes != nil {
		n := int64(*b.MaxBodyBytes)
		p.MaxBodyBytes = &n
	}
	if b.RetentionMaxCount != nil {
		n := int64(*b.RetentionMaxCount)
		p.RetentionMaxCount = &n
	}
	v, err := a.s.adm.Update(ctx, req.Slug, p)
	if err != nil {
		f := a.s.classify(ctx, "update inbox", err)
		switch f.status {
		case http.StatusBadRequest:
			return api.UpdateInbox400JSONResponse{BadRequestJSONResponse: badRequest(ctx, f)}, nil
		case http.StatusNotFound:
			return api.UpdateInbox404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		case http.StatusConflict:
			return api.UpdateInbox409JSONResponse{ConflictJSONResponse: conflict(ctx, f)}, nil
		}
		return api.UpdateInbox500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	a.s.auditCtx(ctx, "update inbox", v.Slug)
	return api.UpdateInbox200JSONResponse{Body: view(v, a.s.baseURLFor(ctx)), Headers: api.UpdateInbox200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *adminAPI) DeleteInbox(ctx context.Context, req api.DeleteInboxRequestObject) (api.DeleteInboxResponseObject, error) {
	if err := a.s.adm.Delete(ctx, req.Slug); err != nil {
		f := a.s.classify(ctx, "delete inbox", err)
		if f.status == http.StatusNotFound {
			return api.DeleteInbox404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		}
		return api.DeleteInbox500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	a.s.auditCtx(ctx, "delete inbox", req.Slug)
	return api.DeleteInbox204Response{Headers: api.DeleteInbox204ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *adminAPI) SetVerifierSecret(ctx context.Context, req api.SetVerifierSecretRequestObject) (api.SetVerifierSecretResponseObject, error) {
	v, err := a.s.adm.SetSecret(ctx, req.Slug, req.Body.Secret)
	if err != nil {
		f := a.s.classify(ctx, "set secret", err)
		switch f.status {
		case http.StatusBadRequest:
			return api.SetVerifierSecret400JSONResponse{BadRequestJSONResponse: badRequest(ctx, f)}, nil
		case http.StatusNotFound:
			return api.SetVerifierSecret404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		case http.StatusConflict:
			return api.SetVerifierSecret409JSONResponse{ConflictJSONResponse: conflict(ctx, f)}, nil
		case http.StatusNotImplemented:
			return api.SetVerifierSecret501JSONResponse{NotImplementedJSONResponse: notImplemented(ctx, f)}, nil
		}
		return api.SetVerifierSecret500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	a.s.auditCtx(ctx, "set verifier secret", v.Slug)
	return api.SetVerifierSecret200JSONResponse{Body: view(v, a.s.baseURLFor(ctx)), Headers: api.SetVerifierSecret200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *adminAPI) CreateToken(ctx context.Context, req api.CreateTokenRequestObject) (api.CreateTokenResponseObject, error) {
	b := req.Body
	c, err := a.s.adm.CreateToken(ctx, admin.TokenParams{Name: b.Name, Inboxes: orZero(b.Inboxes), AllInboxes: orZero(b.AllInboxes)})
	if err != nil {
		f := a.s.classify(ctx, "create token", err)
		switch f.status {
		case http.StatusBadRequest:
			return api.CreateToken400JSONResponse{BadRequestJSONResponse: badRequest(ctx, f)}, nil
		case http.StatusNotFound:
			return api.CreateToken404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		}
		return api.CreateToken500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	a.s.auditCtx(ctx, "create token", "", slog.Int64("token_id", c.Record.ID), slog.String("name", c.Record.Name))
	return api.CreateToken201JSONResponse{
		Body:    api.TokenCreated{Record: tokenView(c.Record), Token: c.Token},
		Headers: api.CreateToken201ResponseHeaders{Location: "/admin/tokens/" + strconv.FormatInt(c.Record.ID, 10), XRequestId: requestIDOf(ctx)},
	}, nil
}

func (a *adminAPI) ListTokens(ctx context.Context, req api.ListTokensRequestObject) (api.ListTokensResponseObject, error) {
	limit, slug := orZero(req.Params.Limit), orZero(req.Params.Inbox)
	views, next, err := a.s.adm.ListTokens(ctx, inbox.TokenID(orZero(req.Params.After)), limit, slug)
	if err != nil {
		f := a.s.classify(ctx, "list tokens", err)
		switch f.status {
		case http.StatusBadRequest:
			return api.ListTokens400JSONResponse{BadRequestJSONResponse: badRequest(ctx, f)}, nil
		case http.StatusNotFound:
			return api.ListTokens404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		}
		return api.ListTokens500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	out := api.TokenList{Tokens: make([]api.Token, len(views))}
	for i, v := range views {
		out.Tokens[i] = tokenView(v)
	}
	if next != 0 {
		q := url.Values{"after": {strconv.FormatInt(int64(next), 10)}}
		if limit > 0 {
			q.Set("limit", strconv.Itoa(limit))
		}
		if slug != "" {
			q.Set("inbox", slug)
		}
		n := "/admin/tokens?" + q.Encode()
		out.Next = &n
	}
	return api.ListTokens200JSONResponse{Body: out, Headers: api.ListTokens200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *adminAPI) RevokeToken(ctx context.Context, req api.RevokeTokenRequestObject) (api.RevokeTokenResponseObject, error) {
	if err := a.s.adm.RevokeToken(ctx, inbox.TokenID(req.Id)); err != nil {
		f := a.s.classify(ctx, "revoke token", err)
		if f.status == http.StatusNotFound {
			return api.RevokeToken404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		}
		return api.RevokeToken500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	a.s.auditCtx(ctx, "revoke token", "", slog.Int("token_id", req.Id))
	return api.RevokeToken204Response{Headers: api.RevokeToken204ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *adminAPI) RunRetention(ctx context.Context, _ api.RunRetentionRequestObject) (api.RunRetentionResponseObject, error) {
	start := time.Now()
	ctx, cancel := a.s.closingContext(ctx)
	defer cancel()
	n, err := a.s.adm.RunRetention(ctx)
	if err != nil {
		f := a.s.classify(ctx, "retention", err)
		if f.status == http.StatusConflict {
			return api.RunRetention409JSONResponse{ConflictJSONResponse: conflict(ctx, f)}, nil
		}
		return api.RunRetention500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	a.s.auditCtx(ctx, "run retention", "", slog.Int64("deleted", n), slog.Duration("took", time.Since(start).Round(time.Millisecond)))
	return api.RunRetention200JSONResponse{Body: api.Deleted{Deleted: int(n)}, Headers: api.RunRetention200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

// Backup streams a snapshot of the whole Store: every inbox, message, and sealed
// secret. Only one runs at a time, since each holds a second copy of the database
// on disk until it has been sent.
func (a *adminAPI) Backup(ctx context.Context, _ api.BackupRequestObject) (api.BackupResponseObject, error) {
	s := a.s
	if !s.backupMu.TryLock() {
		return api.Backup409JSONResponse{ConflictJSONResponse: conflict(ctx, fail{http.StatusConflict, "busy", "a backup is already running", "backup"})}, nil
	}
	start := time.Now()
	bctx, cancel := s.closingContext(ctx)
	snap, size, err := s.adm.Backup(bctx)
	if err != nil {
		cancel()
		s.backupMu.Unlock()
		f := s.classify(ctx, "backup", err)
		switch f.status {
		case http.StatusConflict:
			return api.Backup409JSONResponse{ConflictJSONResponse: conflict(ctx, f)}, nil
		case http.StatusNotImplemented:
			return api.Backup501JSONResponse{NotImplementedJSONResponse: notImplemented(ctx, f)}, nil
		}
		return api.Backup500JSONResponse{InternalJSONResponse: s.fell(ctx, f)}, nil
	}
	// A big database can take longer to send than any write deadline.
	if w, ok := ctx.Value(writerKey).(http.ResponseWriter); ok {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	}
	resp := api.Backup200ApplicationvndSqlite3Response{
		Body: &backupBody{s: s, ctx: bctx, cancel: cancel, snap: snap, size: size, start: start},
		Headers: api.Backup200ResponseHeaders{
			ContentDisposition: `attachment; filename="hookinlet-` + s.opts.Now().UTC().Format("20060102-150405") + `.db"`,
			XRequestId:         requestIDOf(ctx),
		},
	}
	if size >= 0 { // a Store that streams its snapshot may not know how big it will be
		n := int(size)
		resp.ContentLength, resp.Headers.ContentLength = size, &n
	}
	return resp, nil
}

// backupBody is the snapshot being sent. It releases the backup lock when the
// response is finished with it, and records how it went.
type backupBody struct {
	s      *Server
	ctx    context.Context
	cancel context.CancelFunc
	snap   io.ReadCloser
	size   int64
	start  time.Time
	sent   int64
	once   sync.Once
}

func (b *backupBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.snap.Read(p)
	b.sent += int64(n)
	switch {
	case errors.Is(err, io.EOF):
		b.s.auditCtx(b.ctx, "backup", "", slog.Int64("size", b.size), slog.Duration("took", time.Since(b.start).Round(time.Millisecond)))
	case err != nil:
		b.s.log.LogAttrs(b.ctx, slog.LevelWarn, "backup interrupted",
			slog.Int64("sent", b.sent), slog.Int64("size", b.size), slog.String("err", err.Error()))
	}
	return n, err
}

func (b *backupBody) Close() error {
	b.once.Do(func() {
		_ = b.snap.Close()
		b.cancel()
		b.s.backupMu.Unlock()
	})
	return nil
}

func (a *adminAPI) RotateIngestKey(ctx context.Context, req api.RotateIngestKeyRequestObject) (api.RotateIngestKeyResponseObject, error) {
	v, err := a.s.adm.RotateIngestKey(ctx, req.Slug)
	if err != nil {
		f := a.s.classify(ctx, "rotate ingest key", err)
		switch f.status {
		case http.StatusNotFound:
			return api.RotateIngestKey404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, f)}, nil
		case http.StatusConflict:
			return api.RotateIngestKey409JSONResponse{ConflictJSONResponse: conflict(ctx, f)}, nil
		}
		return api.RotateIngestKey500JSONResponse{InternalJSONResponse: a.s.fell(ctx, f)}, nil
	}
	a.s.auditCtx(ctx, "rotate ingest key", v.Slug)
	return api.RotateIngestKey200JSONResponse{Body: view(v, a.s.baseURLFor(ctx)), Headers: api.RotateIngestKey200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}
