// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"net/http"

	"github.com/hookinlet/hookinlet/internal/api"
)

// ingestAPI serves POST /in/{key} and POST /in/{key}/{rest} through the generated
// interface. The delivery itself is Server.ingest; what is here turns how it ended
// into a response the document lists for the operation, so that an ingest cannot
// answer with a status the document does not describe.
type ingestAPI struct {
	s *Server
}

func (a *ingestAPI) run(ctx context.Context) ingestResult {
	r, _ := ctx.Value(requestKey).(*http.Request)
	w, _ := ctx.Value(writerKey).(http.ResponseWriter)
	res := a.s.ingest(ctx, w, r)
	// The delivery may have taken a while to receive and store; the client gets the
	// usual time to take the answer from now.
	if rr, ok := ctx.Value(readKey).(*readRequest); ok && rr.deadline != nil {
		rr.deadline()
	}
	a.s.metrics.delivery(deliveryResult(res))
	return res
}

func (a *ingestAPI) Ingest(ctx context.Context, _ api.IngestRequestObject) (api.IngestResponseObject, error) {
	res := a.run(ctx)
	id := requestIDOf(ctx)
	switch res.status {
	case http.StatusOK:
		if res.challenge != nil {
			setNosniff(ctx)
			return api.Ingest200TextResponse{Body: string(res.challenge), Headers: api.Ingest200ResponseHeaders{XRequestId: id}}, nil
		}
		return api.Ingest200JSONResponse{Body: api.Accepted{Seq: int(res.seq)}, Headers: api.Ingest200ResponseHeaders{XRequestId: id}}, nil
	case http.StatusBadRequest:
		var b api.ErrorBadRequest
		b.Error.Code, b.Error.Message = api.ErrorBadRequestErrorCode(res.code), res.msg
		return api.Ingest400JSONResponse{Body: b, Headers: api.Ingest400ResponseHeaders{XRequestId: id}}, nil
	case http.StatusUnauthorized:
		var b api.ErrorVerificationFailed
		b.Error.Code, b.Error.Message = api.VerificationFailed, res.msg
		return api.Ingest401JSONResponse{Body: b, Headers: api.Ingest401ResponseHeaders{XRequestId: id}}, nil
	case http.StatusPaymentRequired:
		var b api.ErrorQuotaExceeded
		b.Error.Code, b.Error.Message = api.QuotaExceeded, res.msg
		return api.Ingest402JSONResponse{Body: b, Headers: api.Ingest402ResponseHeaders{XRequestId: id}}, nil
	case http.StatusForbidden:
		var b api.ErrorForbidden
		b.Error.Code, b.Error.Message = api.ErrorForbiddenErrorCodeForbidden, res.msg
		return api.Ingest403JSONResponse{Body: b, Headers: api.Ingest403ResponseHeaders{XRequestId: id}}, nil
	case http.StatusNotFound:
		return api.Ingest404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, fail{status: res.status, code: res.code, msg: res.msg})}, nil
	case http.StatusMethodNotAllowed:
		return api.Ingest405JSONResponse{MethodNotAllowedJSONResponse: methodNotAllowed(ctx, "POST, PUT", res.msg)}, nil
	case http.StatusRequestEntityTooLarge:
		return api.Ingest413JSONResponse{TooLargeJSONResponse: tooLarge(ctx, res.msg)}, nil
	case http.StatusTooManyRequests:
		var b api.ErrorRateLimited
		b.Error.Code, b.Error.Message = api.RateLimited, res.msg
		return api.Ingest429JSONResponse{Body: b, Headers: api.Ingest429ResponseHeaders{XRequestId: id}}, nil
	case http.StatusServiceUnavailable:
		return api.Ingest503JSONResponse{OverloadedJSONResponse: overloadedResp(ctx, res)}, nil
	}
	return api.Ingest500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
}

func (a *ingestAPI) IngestWithPath(ctx context.Context, _ api.IngestWithPathRequestObject) (api.IngestWithPathResponseObject, error) {
	res := a.run(ctx)
	id := requestIDOf(ctx)
	switch res.status {
	case http.StatusOK:
		if res.challenge != nil {
			setNosniff(ctx)
			return api.IngestWithPath200TextResponse{Body: string(res.challenge), Headers: api.IngestWithPath200ResponseHeaders{XRequestId: id}}, nil
		}
		return api.IngestWithPath200JSONResponse{Body: api.Accepted{Seq: int(res.seq)}, Headers: api.IngestWithPath200ResponseHeaders{XRequestId: id}}, nil
	case http.StatusBadRequest:
		var b api.ErrorBadRequest
		b.Error.Code, b.Error.Message = api.ErrorBadRequestErrorCode(res.code), res.msg
		return api.IngestWithPath400JSONResponse{Body: b, Headers: api.IngestWithPath400ResponseHeaders{XRequestId: id}}, nil
	case http.StatusUnauthorized:
		var b api.ErrorVerificationFailed
		b.Error.Code, b.Error.Message = api.VerificationFailed, res.msg
		return api.IngestWithPath401JSONResponse{Body: b, Headers: api.IngestWithPath401ResponseHeaders{XRequestId: id}}, nil
	case http.StatusPaymentRequired:
		var b api.ErrorQuotaExceeded
		b.Error.Code, b.Error.Message = api.QuotaExceeded, res.msg
		return api.IngestWithPath402JSONResponse{Body: b, Headers: api.IngestWithPath402ResponseHeaders{XRequestId: id}}, nil
	case http.StatusForbidden:
		var b api.ErrorForbidden
		b.Error.Code, b.Error.Message = api.ErrorForbiddenErrorCodeForbidden, res.msg
		return api.IngestWithPath403JSONResponse{Body: b, Headers: api.IngestWithPath403ResponseHeaders{XRequestId: id}}, nil
	case http.StatusNotFound:
		return api.IngestWithPath404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, fail{status: res.status, code: res.code, msg: res.msg})}, nil
	case http.StatusMethodNotAllowed:
		return api.IngestWithPath405JSONResponse{MethodNotAllowedJSONResponse: methodNotAllowed(ctx, "POST, PUT", res.msg)}, nil
	case http.StatusRequestEntityTooLarge:
		return api.IngestWithPath413JSONResponse{TooLargeJSONResponse: tooLarge(ctx, res.msg)}, nil
	case http.StatusTooManyRequests:
		var b api.ErrorRateLimited
		b.Error.Code, b.Error.Message = api.RateLimited, res.msg
		return api.IngestWithPath429JSONResponse{Body: b, Headers: api.IngestWithPath429ResponseHeaders{XRequestId: id}}, nil
	case http.StatusServiceUnavailable:
		return api.IngestWithPath503JSONResponse{OverloadedJSONResponse: overloadedResp(ctx, res)}, nil
	}
	return api.IngestWithPath500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
}

func (a *ingestAPI) IngestPut(ctx context.Context, _ api.IngestPutRequestObject) (api.IngestPutResponseObject, error) {
	res := a.run(ctx)
	id := requestIDOf(ctx)
	switch res.status {
	case http.StatusOK:
		if res.challenge != nil {
			setNosniff(ctx)
			return api.IngestPut200TextResponse{Body: string(res.challenge), Headers: api.IngestPut200ResponseHeaders{XRequestId: id}}, nil
		}
		return api.IngestPut200JSONResponse{Body: api.Accepted{Seq: int(res.seq)}, Headers: api.IngestPut200ResponseHeaders{XRequestId: id}}, nil
	case http.StatusBadRequest:
		var b api.ErrorBadRequest
		b.Error.Code, b.Error.Message = api.ErrorBadRequestErrorCode(res.code), res.msg
		return api.IngestPut400JSONResponse{Body: b, Headers: api.IngestPut400ResponseHeaders{XRequestId: id}}, nil
	case http.StatusUnauthorized:
		var b api.ErrorVerificationFailed
		b.Error.Code, b.Error.Message = api.VerificationFailed, res.msg
		return api.IngestPut401JSONResponse{Body: b, Headers: api.IngestPut401ResponseHeaders{XRequestId: id}}, nil
	case http.StatusPaymentRequired:
		var b api.ErrorQuotaExceeded
		b.Error.Code, b.Error.Message = api.QuotaExceeded, res.msg
		return api.IngestPut402JSONResponse{Body: b, Headers: api.IngestPut402ResponseHeaders{XRequestId: id}}, nil
	case http.StatusForbidden:
		var b api.ErrorForbidden
		b.Error.Code, b.Error.Message = api.ErrorForbiddenErrorCodeForbidden, res.msg
		return api.IngestPut403JSONResponse{Body: b, Headers: api.IngestPut403ResponseHeaders{XRequestId: id}}, nil
	case http.StatusNotFound:
		return api.IngestPut404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, fail{status: res.status, code: res.code, msg: res.msg})}, nil
	case http.StatusMethodNotAllowed:
		return api.IngestPut405JSONResponse{MethodNotAllowedJSONResponse: methodNotAllowed(ctx, "POST, PUT", res.msg)}, nil
	case http.StatusRequestEntityTooLarge:
		return api.IngestPut413JSONResponse{TooLargeJSONResponse: tooLarge(ctx, res.msg)}, nil
	case http.StatusTooManyRequests:
		var b api.ErrorRateLimited
		b.Error.Code, b.Error.Message = api.RateLimited, res.msg
		return api.IngestPut429JSONResponse{Body: b, Headers: api.IngestPut429ResponseHeaders{XRequestId: id}}, nil
	case http.StatusServiceUnavailable:
		return api.IngestPut503JSONResponse{OverloadedJSONResponse: overloadedResp(ctx, res)}, nil
	}
	return api.IngestPut500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
}

func (a *ingestAPI) IngestPutWithPath(ctx context.Context, _ api.IngestPutWithPathRequestObject) (api.IngestPutWithPathResponseObject, error) {
	res := a.run(ctx)
	id := requestIDOf(ctx)
	switch res.status {
	case http.StatusOK:
		if res.challenge != nil {
			setNosniff(ctx)
			return api.IngestPutWithPath200TextResponse{Body: string(res.challenge), Headers: api.IngestPutWithPath200ResponseHeaders{XRequestId: id}}, nil
		}
		return api.IngestPutWithPath200JSONResponse{Body: api.Accepted{Seq: int(res.seq)}, Headers: api.IngestPutWithPath200ResponseHeaders{XRequestId: id}}, nil
	case http.StatusBadRequest:
		var b api.ErrorBadRequest
		b.Error.Code, b.Error.Message = api.ErrorBadRequestErrorCode(res.code), res.msg
		return api.IngestPutWithPath400JSONResponse{Body: b, Headers: api.IngestPutWithPath400ResponseHeaders{XRequestId: id}}, nil
	case http.StatusUnauthorized:
		var b api.ErrorVerificationFailed
		b.Error.Code, b.Error.Message = api.VerificationFailed, res.msg
		return api.IngestPutWithPath401JSONResponse{Body: b, Headers: api.IngestPutWithPath401ResponseHeaders{XRequestId: id}}, nil
	case http.StatusPaymentRequired:
		var b api.ErrorQuotaExceeded
		b.Error.Code, b.Error.Message = api.QuotaExceeded, res.msg
		return api.IngestPutWithPath402JSONResponse{Body: b, Headers: api.IngestPutWithPath402ResponseHeaders{XRequestId: id}}, nil
	case http.StatusForbidden:
		var b api.ErrorForbidden
		b.Error.Code, b.Error.Message = api.ErrorForbiddenErrorCodeForbidden, res.msg
		return api.IngestPutWithPath403JSONResponse{Body: b, Headers: api.IngestPutWithPath403ResponseHeaders{XRequestId: id}}, nil
	case http.StatusNotFound:
		return api.IngestPutWithPath404JSONResponse{NotFoundJSONResponse: notFoundResp(ctx, fail{status: res.status, code: res.code, msg: res.msg})}, nil
	case http.StatusMethodNotAllowed:
		return api.IngestPutWithPath405JSONResponse{MethodNotAllowedJSONResponse: methodNotAllowed(ctx, "POST, PUT", res.msg)}, nil
	case http.StatusRequestEntityTooLarge:
		return api.IngestPutWithPath413JSONResponse{TooLargeJSONResponse: tooLarge(ctx, res.msg)}, nil
	case http.StatusTooManyRequests:
		var b api.ErrorRateLimited
		b.Error.Code, b.Error.Message = api.RateLimited, res.msg
		return api.IngestPutWithPath429JSONResponse{Body: b, Headers: api.IngestPutWithPath429ResponseHeaders{XRequestId: id}}, nil
	case http.StatusServiceUnavailable:
		return api.IngestPutWithPath503JSONResponse{OverloadedJSONResponse: overloadedResp(ctx, res)}, nil
	}
	return api.IngestPutWithPath500JSONResponse{InternalJSONResponse: internal(ctx)}, nil
}

// setNosniff marks the handshake answer, which echoes what the sender asked for,
// as text that no browser may guess a type for.
func setNosniff(ctx context.Context) {
	if w, ok := ctx.Value(writerKey).(http.ResponseWriter); ok {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
}

func methodNotAllowed(ctx context.Context, allow, msg string) api.MethodNotAllowedJSONResponse {
	var b api.ErrorMethodNotAllowed
	b.Error.Code, b.Error.Message = api.ErrorMethodNotAllowedErrorCodeMethodNotAllowed, msg
	return api.MethodNotAllowedJSONResponse{Body: b, Headers: api.MethodNotAllowedResponseHeaders{Allow: allow, XRequestId: requestIDOf(ctx)}}
}

func tooLarge(ctx context.Context, msg string) api.TooLargeJSONResponse {
	var b api.ErrorTooLarge
	b.Error.Code, b.Error.Message = api.PayloadTooLarge, msg
	return api.TooLargeJSONResponse{Body: b, Headers: api.TooLargeResponseHeaders{XRequestId: requestIDOf(ctx)}}
}

// overloadedResp is the 503 for a delivery the server cannot take now.
func overloadedResp(ctx context.Context, res ingestResult) api.OverloadedJSONResponse {
	var b api.ErrorOverloaded
	b.Error.Code, b.Error.Message = api.ErrorOverloadedErrorCode(res.code), res.msg
	return api.OverloadedJSONResponse{Body: b, Headers: api.OverloadedResponseHeaders{RetryAfter: "30", XRequestId: requestIDOf(ctx)}}
}
