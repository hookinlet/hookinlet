// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/hookinlet/hookinlet/internal/api"
	"github.com/hookinlet/hookinlet/internal/wire"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// readAPI serves the read operations that return one inbox's data, through the
// generated interface: what each may answer is what the document lists for it.
//
// The query string is parsed by internal/wire, not taken from the generated
// parameters: wire is also what the stream and MCP use, and the guard has already
// held the request to the document, so the generated binding adds nothing but a
// second parse. The message bodies are carried as raw JSON (see
// internal/api/overlay.yaml), so what reaches the reader is what the sender sent.
type readAPI struct {
	s *Server
}

// readFail is a failed read, classified.
type readFail struct {
	status int
	code   string
	msg    string
}

func (s *Server) classifyRead(ctx context.Context, what string, err error) readFail {
	switch {
	case errors.Is(err, inbox.ErrInvalid):
		return readFail{http.StatusBadRequest, "invalid_parameter", err.Error()}
	case errors.Is(err, inbox.ErrNotFound):
		return readFail{http.StatusNotFound, "not_found", "not found"}
	}
	level := slog.LevelError
	if errors.Is(err, context.Canceled) {
		level = slog.LevelDebug
	}
	s.log.LogAttrs(ctx, level, "request failed", slog.String("op", what), slog.String("err", err.Error()),
		slog.String("path", readFrom(ctx).r.Pattern))
	return readFail{http.StatusInternalServerError, "internal", "internal error"}
}

func (rf readFail) badRequest(ctx context.Context) api.BadRequestJSONResponse {
	return badRequest(ctx, fail{status: rf.status, code: rf.code, msg: rf.msg})
}

func (rf readFail) notFound(ctx context.Context) api.NotFoundJSONResponse {
	return notFoundResp(ctx, fail{status: rf.status, code: rf.code, msg: rf.msg})
}

// message converts a message to the document's type. Every field is named, so
// one the document does not have cannot be sent by accident.
func message(m wire.Message) api.Message {
	return api.Message{
		Body: m.Body, BodyBase64: m.BodyBase64, BodyText: m.BodyText, ContentType: m.ContentType,
		DeliveryId: m.DeliveryID, EventType: m.EventType, Headers: m.Headers, Inbox: m.Inbox,
		Method: api.MessageMethod(m.Method), Path: m.Path, Query: m.Query, ReceivedAt: m.ReceivedAt,
		Seq: int(m.Seq), Size: int(m.Size), SourceIp: m.SourceIP, Verified: m.Verified,
		Verifier: m.Verifier, VerifyError: m.VerifyError,
	}
}

func page(p *wire.Page) api.MessagePage {
	out := api.MessagePage{LatestSeq: int(p.LatestSeq), Next: p.Next, Messages: make([]api.Message, len(p.Messages))}
	for i, m := range p.Messages {
		out.Messages[i] = message(m)
	}
	return out
}

// head sets the headers a GET would have had, so a HEAD tells the caller how big
// the answer is.
func head(rr *readRequest, body any) {
	n, err := api.EncodedLength(body)
	if err != nil {
		return
	}
	h := rr.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(n))
}

// ---- inbox info ----

func (a *readAPI) info(ctx context.Context) (api.InboxInfo, *readFail) {
	rr := readFrom(ctx)
	if values, err := wire.QueryValues(rr.r.URL.RawQuery); err != nil {
		return api.InboxInfo{}, &readFail{http.StatusBadRequest, "invalid_parameter", err.Error()}
	} else if err := wire.RejectParams(values); err != nil {
		return api.InboxInfo{}, &readFail{http.StatusBadRequest, "invalid_parameter", err.Error()}
	}
	info, err := wire.FetchInfo(ctx, a.s.opts.Store, rr.in)
	if err != nil {
		f := a.s.classifyRead(ctx, "inbox info", err)
		return api.InboxInfo{}, &f
	}
	return api.InboxInfo{
		LatestSeq: int(info.LatestSeq), MessageCount: int(info.MessageCount), Name: info.Name,
		OldestSeq: int(info.OldestSeq), Slug: info.Slug, Verifier: info.Verifier,
	}, nil
}

func (a *readAPI) GetInbox(ctx context.Context, _ api.GetInboxRequestObject) (api.GetInboxResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.GetInbox503JSONResponse{OverloadedJSONResponse: *busy}, nil
	}
	defer release()
	info, f := a.info(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.GetInbox400JSONResponse{BadRequestJSONResponse: f.badRequest(ctx)}, nil
		case http.StatusNotFound:
			return api.GetInbox404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.GetInbox500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	return api.GetInbox200JSONResponse{Body: info, Headers: api.GetInbox200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *readAPI) HeadGetInbox(ctx context.Context, _ api.HeadGetInboxRequestObject) (api.HeadGetInboxResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.HeadGetInbox503Response{Headers: api.HeadGetInbox503ResponseHeaders{RetryAfter: "1", XRequestId: requestIDOf(ctx)}}, nil
	}
	defer release()
	info, f := a.info(ctx)
	id := requestIDOf(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.HeadGetInbox400Response{Headers: api.HeadGetInbox400ResponseHeaders{XRequestId: id}}, nil
		case http.StatusNotFound:
			return api.HeadGetInbox404Response{Headers: api.HeadGetInbox404ResponseHeaders{XRequestId: id}}, nil
		}
		return api.HeadGetInbox500Response{Headers: api.HeadGetInbox500ResponseHeaders{XRequestId: id}}, nil
	}
	head(readFrom(ctx), info)
	return api.HeadGetInbox200Response{Headers: api.HeadGetInbox200ResponseHeaders{XRequestId: id}}, nil
}

// ---- messages ----

func (a *readAPI) messages(ctx context.Context) (api.MessagePage, *readFail) {
	rr := readFrom(ctx)
	values, err := wire.QueryValues(rr.r.URL.RawQuery)
	var req wire.PageRequest
	if err == nil {
		req, err = wire.ParseMessages(values)
	}
	if err != nil {
		return api.MessagePage{}, &readFail{http.StatusBadRequest, "invalid_parameter", err.Error()}
	}
	p, err := wire.FetchPage(ctx, a.s.opts.Store, rr.in, req)
	if err != nil {
		f := a.s.classifyRead(ctx, "messages", err)
		return api.MessagePage{}, &f
	}
	return page(p), nil
}

func (a *readAPI) ListMessages(ctx context.Context, _ api.ListMessagesRequestObject) (api.ListMessagesResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.ListMessages503JSONResponse{OverloadedJSONResponse: *busy}, nil
	}
	defer release()
	p, f := a.messages(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.ListMessages400JSONResponse{BadRequestJSONResponse: f.badRequest(ctx)}, nil
		case http.StatusNotFound:
			return api.ListMessages404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.ListMessages500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	return api.ListMessages200JSONResponse{Body: p, Headers: api.ListMessages200ResponseHeaders{XRequestId: requestIDOf(ctx)}}, nil
}

func (a *readAPI) HeadListMessages(ctx context.Context, _ api.HeadListMessagesRequestObject) (api.HeadListMessagesResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.HeadListMessages503Response{Headers: api.HeadListMessages503ResponseHeaders{RetryAfter: "1", XRequestId: requestIDOf(ctx)}}, nil
	}
	defer release()
	p, f := a.messages(ctx)
	id := requestIDOf(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.HeadListMessages400Response{Headers: api.HeadListMessages400ResponseHeaders{XRequestId: id}}, nil
		case http.StatusNotFound:
			return api.HeadListMessages404Response{Headers: api.HeadListMessages404ResponseHeaders{XRequestId: id}}, nil
		}
		return api.HeadListMessages500Response{Headers: api.HeadListMessages500ResponseHeaders{XRequestId: id}}, nil
	}
	head(readFrom(ctx), p)
	return api.HeadListMessages200Response{Headers: api.HeadListMessages200ResponseHeaders{XRequestId: id}}, nil
}

// ---- one message ----

// load reads the seq from the path and loads that message. A seq that cannot be a
// sequence number is simply a message that does not exist.
func (a *readAPI) load(ctx context.Context) (*inbox.Message, *readFail) {
	rr := readFrom(ctx)
	if values, err := wire.QueryValues(rr.r.URL.RawQuery); err != nil {
		return nil, &readFail{http.StatusBadRequest, "invalid_parameter", err.Error()}
	} else if err := wire.RejectParams(values); err != nil {
		return nil, &readFail{http.StatusBadRequest, "invalid_parameter", err.Error()}
	}
	raw := rr.r.PathValue("seq")
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq < 1 || strconv.FormatInt(seq, 10) != raw {
		return nil, &readFail{http.StatusNotFound, "not_found", "not found"}
	}
	m, err := a.s.opts.Store.Get(ctx, rr.in.ID, seq)
	if err != nil {
		f := a.s.classifyRead(ctx, "message", err)
		return nil, &f
	}
	return m, nil
}

func (a *readAPI) GetMessage(ctx context.Context, _ api.GetMessageRequestObject) (api.GetMessageResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.GetMessage503JSONResponse{OverloadedJSONResponse: *busy}, nil
	}
	defer release()
	m, f := a.load(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.GetMessage400JSONResponse{BadRequestJSONResponse: f.badRequest(ctx)}, nil
		case http.StatusNotFound:
			return api.GetMessage404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.GetMessage500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	in := readFrom(ctx).in
	return api.GetMessage200JSONResponse{
		Body:    message(wire.FromStored(m, in.Slug, in.Verifier, true)),
		Headers: api.GetMessage200ResponseHeaders{XRequestId: requestIDOf(ctx)},
	}, nil
}

func (a *readAPI) HeadGetMessage(ctx context.Context, _ api.HeadGetMessageRequestObject) (api.HeadGetMessageResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.HeadGetMessage503Response{Headers: api.HeadGetMessage503ResponseHeaders{RetryAfter: "1", XRequestId: requestIDOf(ctx)}}, nil
	}
	defer release()
	m, f := a.load(ctx)
	id := requestIDOf(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.HeadGetMessage400Response{Headers: api.HeadGetMessage400ResponseHeaders{XRequestId: id}}, nil
		case http.StatusNotFound:
			return api.HeadGetMessage404Response{Headers: api.HeadGetMessage404ResponseHeaders{XRequestId: id}}, nil
		}
		return api.HeadGetMessage500Response{Headers: api.HeadGetMessage500ResponseHeaders{XRequestId: id}}, nil
	}
	rr := readFrom(ctx)
	head(rr, message(wire.FromStored(m, rr.in.Slug, rr.in.Verifier, true)))
	return api.HeadGetMessage200Response{Headers: api.HeadGetMessage200ResponseHeaders{XRequestId: id}}, nil
}

// ---- the exact bytes ----

// GetMessageRaw returns the bytes that were received, with the Content-Type the
// sender used.
//
// Those bytes and that type are chosen by whoever sent the webhook, so they are
// served as something to download and never as a page: nosniff stops a browser
// from guessing a more dangerous type, the sandbox policy neutralizes any script
// in HTML or SVG that does get rendered, and attachment tells it to save the file
// instead. None of this changes the body or the Content-Type.
func (a *readAPI) GetMessageRaw(ctx context.Context, _ api.GetMessageRawRequestObject) (api.GetMessageRawResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.GetMessageRaw503JSONResponse{OverloadedJSONResponse: *busy}, nil
	}
	defer release()
	m, f := a.load(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.GetMessageRaw400JSONResponse{BadRequestJSONResponse: f.badRequest(ctx)}, nil
		case http.StatusNotFound:
			return api.GetMessageRaw404JSONResponse{NotFoundJSONResponse: f.notFound(ctx)}, nil
		}
		return api.GetMessageRaw500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
	}
	return api.GetMessageRaw200AsteriskResponse{
		Body:        bytes.NewReader(m.Body),
		ContentType: rawContentType(m),
		Headers: api.GetMessageRaw200ResponseHeaders{
			ContentDisposition: "attachment", ContentLength: len(m.Body),
			ContentSecurityPolicy: "default-src 'none'; sandbox", CrossOriginResourcePolicy: "same-origin",
			XContentTypeOptions: "nosniff", XRequestId: requestIDOf(ctx),
		},
	}, nil
}

func (a *readAPI) HeadGetMessageRaw(ctx context.Context, _ api.HeadGetMessageRawRequestObject) (api.HeadGetMessageRawResponseObject, error) {
	// Only authenticated, authorized requests take a slot, so anonymous traffic
	// cannot starve real readers.
	release, busy, err := a.slot(ctx)
	if err != nil {
		return nil, err
	}
	if busy != nil {
		return api.HeadGetMessageRaw503Response{Headers: api.HeadGetMessageRaw503ResponseHeaders{RetryAfter: "1", XRequestId: requestIDOf(ctx)}}, nil
	}
	defer release()
	m, f := a.load(ctx)
	id := requestIDOf(ctx)
	if f != nil {
		switch f.status {
		case http.StatusBadRequest:
			return api.HeadGetMessageRaw400Response{Headers: api.HeadGetMessageRaw400ResponseHeaders{XRequestId: id}}, nil
		case http.StatusNotFound:
			return api.HeadGetMessageRaw404Response{Headers: api.HeadGetMessageRaw404ResponseHeaders{XRequestId: id}}, nil
		}
		return api.HeadGetMessageRaw500Response{Headers: api.HeadGetMessageRaw500ResponseHeaders{XRequestId: id}}, nil
	}
	// The generated HEAD response has no content type to carry, and the type is
	// the sender's, so it is set on the writer.
	readFrom(ctx).w.Header().Set("Content-Type", rawContentType(m))
	return api.HeadGetMessageRaw200Response{Headers: api.HeadGetMessageRaw200ResponseHeaders{
		ContentDisposition: "attachment", ContentLength: len(m.Body),
		ContentSecurityPolicy: "default-src 'none'; sandbox", CrossOriginResourcePolicy: "same-origin",
		XContentTypeOptions: "nosniff", XRequestId: id,
	}}, nil
}

func rawContentType(m *inbox.Message) string {
	if m.ContentType != "" {
		return m.ContentType
	}
	return "application/octet-stream"
}

// slot takes one of the limited read slots, waiting up to ReadWaitTimeout. busy is
// the answer when none came free; err is set when the caller went away first, and
// then there is nobody to answer.
func (a *readAPI) slot(ctx context.Context) (release func(), busy *api.OverloadedJSONResponse, err error) {
	release, err = a.s.readSlot(ctx)
	switch {
	case err == nil:
		return release, nil, nil
	case errors.Is(err, errBusy):
		b := overloaded(ctx, "1", "the server is busy; retry shortly")
		return nil, &b, nil
	}
	return nil, nil, err
}

func overloaded(ctx context.Context, retryAfter, msg string) api.OverloadedJSONResponse {
	var b api.ErrorOverloaded
	b.Error.Code, b.Error.Message = api.ErrorOverloadedErrorCodeOverloaded, msg
	return api.OverloadedJSONResponse{Body: b, Headers: api.OverloadedResponseHeaders{RetryAfter: retryAfter, XRequestId: requestIDOf(ctx)}}
}
