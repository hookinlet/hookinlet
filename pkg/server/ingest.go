// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hookinlet/hookinlet/internal/jsonptr"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// maxEventTypeLen bounds an event type taken from a delivery.
const maxEventTypeLen = 256

// ingestResult is how a delivery ended. ingestAPI turns it into one of the
// responses the document lists for the operation.
type ingestResult struct {
	status    int
	code, msg string
	seq       int64
	challenge []byte // set for a handshake answered instead of stored
}

func refuse(status int, code, msg string) ingestResult {
	return ingestResult{status: status, code: code, msg: msg}
}

var errInternal = ingestResult{status: http.StatusInternalServerError, code: "internal", msg: "internal error"}

// internalFailure logs the real cause and tells the client nothing about it. A
// request whose client has already gone away is not a fault on our side, so it is
// logged quietly.
func (s *Server) internalFailure(ctx context.Context, what string, err error, attrs ...slog.Attr) ingestResult {
	attrs = append([]slog.Attr{slog.String("op", what), slog.String("err", err.Error())}, attrs...)
	if errors.Is(err, context.Canceled) {
		s.log.LogAttrs(ctx, slog.LevelDebug, "request abandoned by the client", attrs...)
	} else {
		s.log.LogAttrs(ctx, slog.LevelError, "request failed", attrs...)
	}
	return errInternal
}

// ingest receives a webhook: POST or PUT /in/{key}[/...].
//
// The order of steps matters. The method is checked before the inbox is looked
// up, so a wrong method never reveals whether an inbox exists. The body is read
// only under a size limit. Nothing is answered 2xx until the message is
// committed to the Store (guardrail 16).
func (s *Server) ingest(ctx context.Context, w http.ResponseWriter, r *http.Request) ingestResult {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		return refuse(http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint accepts POST and PUT")
	}

	// 1. Find the inbox. An unknown key and a disabled inbox look the same.
	in, err := s.opts.Store.ResolveIngestKey(ctx, r.PathValue("key"))
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		return refuse(http.StatusNotFound, "not_found", "not found")
	case err != nil:
		return s.internalFailure(ctx, "resolve ingest key", err)
	case in.Disabled:
		return refuse(http.StatusNotFound, "not_found", "not found")
	}

	// 2. Read the body, never more than the inbox allows.
	if r.ContentLength > in.MaxBodyBytes {
		return refuse(http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
	}
	// A sender gets a bounded time to deliver the body, so a trickle cannot hold a
	// connection and its buffer open indefinitely. Servers that cannot set a
	// deadline (such as a test recorder) just do without one.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.opts.ReadBodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, in.MaxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return refuse(http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
	case err != nil:
		return refuse(http.StatusBadRequest, "bad_request", "the request body could not be read")
	}

	// 3. Let the embedder reject the delivery (rate limit, quota, policy).
	if err := s.opts.Hooks.BeforeIngest(ctx, in, int64(len(body))); err != nil {
		return s.rejected(ctx, in, err)
	}

	// 4. Verify the signature over the exact bytes received.
	verifier, err := s.opts.Verifiers.New(in.Verifier, in.VerifierConfig)
	if err != nil {
		return s.internalFailure(ctx, "build verifier", err, slog.String("verifier", in.Verifier))
	}
	secret, err := s.verifierSecret(in)
	if err != nil {
		return s.internalFailure(ctx, "open verifier secret", err)
	}
	// The decrypted secret is wiped when verification ends, however it ends.
	res, verr := func() (verify.Result, error) {
		defer clear(secret)
		return verifier.Verify(r, body, secret, s.opts.Now())
	}()

	verified, verifyReason := true, ""
	if verr != nil {
		reason, isFailure := verify.IsFailure(verr)
		switch {
		case !isFailure:
			return s.internalFailure(ctx, "verify", verr, slog.String("verifier", in.Verifier))
		case in.OnVerifyFail == inbox.OnVerifyFailStore:
			// Keep the delivery, marked unverified. Nothing the sender claimed
			// is trusted: the failed Result carries no id or type.
			verified, verifyReason, res = false, reason, verify.Result{}
		default:
			// The sender is told only that verification failed. The reason can
			// name the header and scheme an inbox expects, which is not for
			// whoever happens to hold the ingest key; it goes to the log.
			s.log.LogAttrs(ctx, slog.LevelInfo, "delivery rejected",
				slog.String("slug", in.Slug), slog.String("reason", reason), slog.Int("size", len(body)))
			return refuse(http.StatusUnauthorized, "verification_failed", "webhook verification failed")
		}
	}

	// 5. A handshake (Slack's url_verification) is answered, not stored.
	if res.Challenge != nil {
		return ingestResult{status: http.StatusOK, challenge: res.Challenge}
	}

	// 6. Store it. Only once this returns is the delivery safe to acknowledge.
	var redact []string
	if hr, ok := verifier.(verify.HeaderRedactor); ok {
		redact = hr.SecretHeaders()
	}
	eventType := res.EventType
	// The pointer reads the body, which is only worth trusting, or even parsing,
	// once the delivery has verified.
	if eventType == "" && verified && in.EventTypePointer != "" {
		if v, ok := jsonptr.String(body, in.EventTypePointer); ok && v != "" && len(v) <= maxEventTypeLen {
			eventType = v
		}
	}
	msg, created, err := s.opts.Store.Append(ctx, in.ID, &inbox.NewMessage{
		Method:      r.Method,
		PathSuffix:  r.PathValue("rest"),
		Query:       r.URL.RawQuery,
		Headers:     inbox.RedactHeaders(r.Header, redact...),
		ContentType: r.Header.Get("Content-Type"),
		Body:        body,
		Verified:    verified,
		VerifyError: verifyReason,
		DeliveryID:  res.DeliveryID,
		EventType:   eventType,
		SourceIP:    clientIP(r, s.opts.TrustedProxies),
	})
	switch {
	case errors.Is(err, inbox.ErrNotFound): // deleted while we were verifying
		return refuse(http.StatusNotFound, "not_found", "not found")
	case err != nil:
		return s.internalFailure(ctx, "append", err)
	}

	// 7. Committed. Tell the world, then acknowledge. Nothing after the commit
	// may turn into an error response: the sender would retry a delivery we
	// already hold.
	s.afterCommit(ctx, in, msg, created)
	s.log.LogAttrs(ctx, slog.LevelInfo, "delivery",
		slog.String("slug", in.Slug), slog.Int64("seq", msg.Seq), slog.Int("size", len(body)),
		slog.Bool("verified", verified), slog.Bool("duplicate", !created))
	return ingestResult{status: http.StatusOK, seq: msg.Seq}
}

// rejected is the answer to a delivery that BeforeIngest refused.
func (s *Server) rejected(ctx context.Context, in *inbox.Inbox, err error) ingestResult {
	var res ingestResult
	switch {
	case errors.Is(err, inbox.ErrRateLimited):
		res = refuse(http.StatusTooManyRequests, "rate_limited", "too many requests")
	case errors.Is(err, inbox.ErrQuotaExceeded):
		res = refuse(http.StatusPaymentRequired, "quota_exceeded", "the quota for this inbox has been reached")
	case errors.Is(err, inbox.ErrForbidden):
		res = refuse(http.StatusForbidden, "forbidden", "this delivery is not allowed")
	default:
		return s.internalFailure(ctx, "before-ingest hook", err, slog.String("slug", in.Slug))
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "delivery refused by hook",
		slog.String("slug", in.Slug), slog.String("reason", err.Error()))
	return res
}

// afterCommit runs the post-commit notifications. A panic in an embedder's
// code is logged and swallowed, because by now the message is stored.
func (s *Server) afterCommit(ctx context.Context, in *inbox.Inbox, msg *inbox.Message, created bool) {
	defer func() {
		if rec := recover(); rec != nil {
			s.log.LogAttrs(ctx, slog.LevelError, "panic after commit",
				slog.String("slug", in.Slug), slog.Int64("seq", msg.Seq), slog.String("panic", fmt.Sprint(rec)))
		}
	}()
	if created {
		// By now the request that stored the message may be finished and its
		// context cancelled; a notifier that honored that would drop the signal.
		s.opts.Notifier.Publish(context.WithoutCancel(ctx), in.ID, msg.Seq)
	}
	s.opts.Hooks.AfterIngest(ctx, in, msg, created)
}

// verifierSecret returns the inbox's decrypted verifier secret, or nil if it
// has none. The caller should clear the result when done.
func (s *Server) verifierSecret(in *inbox.Inbox) ([]byte, error) {
	if len(in.VerifierSecret) == 0 {
		return nil, nil
	}
	if s.opts.SecretBox == nil {
		return nil, errors.New("the inbox has a verifier secret but the server has no SecretBox")
	}
	secret, err := s.opts.SecretBox.Open(in.VerifierSecret)
	if err != nil {
		return nil, fmt.Errorf("the stored verifier secret cannot be opened (was the master key changed?): %w", err)
	}
	return secret, nil
}

// internalError logs the real cause and tells the client nothing about it. A
// request whose client has already gone away is not a fault on our side, so it
// is logged quietly and not answered.
func (s *Server) internalError(ctx context.Context, w http.ResponseWriter, what string, err error, attrs ...slog.Attr) {
	attrs = append([]slog.Attr{slog.String("op", what), slog.String("err", err.Error())}, attrs...)
	if errors.Is(err, context.Canceled) {
		s.log.LogAttrs(ctx, slog.LevelDebug, "request abandoned by the client", attrs...)
		return
	}
	s.log.LogAttrs(ctx, slog.LevelError, "request failed", attrs...)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}
