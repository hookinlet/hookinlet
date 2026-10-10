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

// handleIngest receives a webhook: POST or PUT /in/{key}[/...].
//
// The order of steps matters. The method is checked before the inbox is looked
// up, so a wrong method never reveals whether an inbox exists. The body is read
// only under a size limit. Nothing is answered 2xx until the message is
// committed to the Store (guardrail 16).
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this endpoint accepts POST and PUT")
		return
	}
	ctx := r.Context()

	// 1. Find the inbox. An unknown key and a disabled inbox look the same.
	in, err := s.opts.Store.ResolveIngestKey(ctx, r.PathValue("key"))
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	case err != nil:
		s.internalError(ctx, w, "resolve ingest key", err)
		return
	case in.Disabled:
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}

	// 2. Read the body, never more than the inbox allows.
	if r.ContentLength > in.MaxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return
	}
	// A sender gets a bounded time to deliver the body, so a trickle cannot hold a
	// connection and its buffer open indefinitely. Servers that cannot set a
	// deadline (such as a test recorder) just do without one.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.opts.ReadBodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, in.MaxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "the request body could not be read")
		return
	}

	// 3. Let the embedder reject the delivery (rate limit, quota, policy).
	if err := s.opts.Hooks.BeforeIngest(ctx, in, int64(len(body))); err != nil {
		s.rejected(ctx, w, in, err)
		return
	}

	// 4. Verify the signature over the exact bytes received.
	verifier, err := s.opts.Verifiers.New(in.Verifier, in.VerifierConfig)
	if err != nil {
		s.internalError(ctx, w, "build verifier", err, slog.String("verifier", in.Verifier))
		return
	}
	secret, err := s.verifierSecret(in)
	if err != nil {
		s.internalError(ctx, w, "open verifier secret", err)
		return
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
			s.internalError(ctx, w, "verify", verr, slog.String("verifier", in.Verifier))
			return
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
			writeError(w, http.StatusUnauthorized, "verification_failed", "webhook verification failed")
			return
		}
	}

	// 5. A handshake (Slack's url_verification) is answered, not stored.
	if res.Challenge != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.Challenge)
		return
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
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	case err != nil:
		s.internalError(ctx, w, "append", err)
		return
	}

	// 7. Committed. Tell the world, then acknowledge. Nothing after the commit
	// may turn into an error response: the sender would retry a delivery we
	// already hold.
	s.afterCommit(ctx, in, msg, created)
	s.log.LogAttrs(ctx, slog.LevelInfo, "delivery",
		slog.String("slug", in.Slug), slog.Int64("seq", msg.Seq), slog.Int("size", len(body)),
		slog.Bool("verified", verified), slog.Bool("duplicate", !created))
	writeJSON(w, http.StatusOK, map[string]int64{"seq": msg.Seq})
}

// rejected answers a delivery that BeforeIngest refused.
func (s *Server) rejected(ctx context.Context, w http.ResponseWriter, in *inbox.Inbox, err error) {
	switch {
	case errors.Is(err, inbox.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
	case errors.Is(err, inbox.ErrQuotaExceeded):
		writeError(w, http.StatusPaymentRequired, "quota_exceeded", "the quota for this inbox has been reached")
	case errors.Is(err, inbox.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "this delivery is not allowed")
	default:
		s.internalError(ctx, w, "before-ingest hook", err, slog.String("slug", in.Slug))
		return
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "delivery refused by hook",
		slog.String("slug", in.Slug), slog.String("reason", err.Error()))
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
		s.opts.Notifier.Publish(ctx, in.ID, msg.Seq)
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
