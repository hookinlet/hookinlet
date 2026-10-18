// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hookinlet/hookinlet/internal/jsonptr"
	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// maxEventTypeLen bounds an event type taken from a delivery.
const maxEventTypeLen = 256

// codeUnverifiedLimit marks, inside the server, the 429 for an inbox at its cap of
// unverified messages. On the wire it is rate_limited, like any other refusal to
// take a delivery for now; metrics tell the two apart.
const codeUnverifiedLimit = "unverified_limit"

// inboxAttrs names an inbox in a log line: its slug, then its tenant when it has
// one, then the rest. A slug is unique only within a tenant, so on a server that
// holds several tenants' inboxes the slug alone does not say which inbox a line is
// about; an installation without tenants has only the empty one and sees no change.
func inboxAttrs(in *inbox.Inbox, more ...slog.Attr) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(more)+2)
	attrs = append(attrs, slog.String("slug", in.Slug))
	if in.Tenant != "" {
		attrs = append(attrs, slog.String("tenant", in.Tenant))
	}
	return append(attrs, more...)
}

// ingestResult is how a delivery ended. ingestAPI turns it into one of the
// responses the document lists for the operation.
type ingestResult struct {
	status    int
	code, msg string
	seq       int64
	challenge []byte // set for a handshake answered instead of stored
	duplicate bool   // a repeat of a delivery already stored
	verified  bool   // the stored message (or the one it repeats) is verified
	// verifyFailure is how verification failed, for a delivery refused with 401.
	verifyFailure inbox.VerifyFailure
	size          int64 // body bytes read
	// retryAfter is the Retry-After for a refusal by BeforeIngest that said when to
	// retry (see retryAfterOf), or "".
	retryAfter string
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
// committed to the Store.
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
	}
	// From here the delivery has an inbox to be counted against, so an observer
	// hears how it ends; before here it has none, and is not observed.
	res := s.deliver(ctx, w, r, in)
	if s.observer != nil {
		s.observe(ctx, in, r, res)
	}
	return res
}

// deliver takes a delivery to a known inbox, from reading its body to storing it.
func (s *Server) deliver(ctx context.Context, w http.ResponseWriter, r *http.Request, in *inbox.Inbox) (out ingestResult) {
	if in.Disabled {
		return refuse(http.StatusNotFound, "not_found", "not found")
	}
	var body []byte
	defer func() { out.size = int64(len(body)) }()

	// 2. Read the body, never more than the inbox allows.
	if r.ContentLength > in.MaxBodyBytes {
		return refuse(http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
	}
	// A sender gets a bounded time to deliver the body, so a trickle cannot hold a
	// connection and its buffer open indefinitely. Servers that cannot set a
	// deadline (such as a test recorder) just do without one.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.opts.ReadBodyTimeout))
	// The body is held in memory until the delivery is stored, whoever sent it, and
	// the signature has not been checked yet. How much may be held at once is
	// bounded: the bytes are counted as they arrive, and a delivery that would take
	// the total over is told to come back.
	br := &budgetReader{r: http.MaxBytesReader(w, r.Body, in.MaxBodyBytes), b: &s.ingestBudget, id: in.ID, share: s.ingestBudget.perInbox(in.MaxBodyBytes)}
	defer br.release()
	body, err := io.ReadAll(br)
	s.metrics.bodyBytes.Add(int64(len(body)))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, errOverBudget):
		return refuse(http.StatusServiceUnavailable, "overloaded", "too many deliveries are being received at once; retry shortly")
	case errors.As(err, &tooLarge):
		return refuse(http.StatusRequestEntityTooLarge, "payload_too_large", "the request body is too large")
	case err != nil:
		return refuse(http.StatusBadRequest, "bad_request", "the request body could not be read")
	}

	// 3. Let the embedder reject the delivery (rate limit, quota, policy).
	if err := s.opts.Hooks.BeforeIngest(ctx, &inbox.IngestRequest{
		Inbox: in, Size: int64(len(body)), SourceIP: clientIP(r, s.opts.TrustedProxies),
		Host: requestHost(r), Method: r.Method, PathSuffix: r.PathValue("rest"), Headers: r.Header,
	}); err != nil {
		return s.rejected(ctx, in, err)
	}

	// 4. Verify the signature over the exact bytes received.
	verifier, err := s.opts.Verifiers.New(in.Verifier, in.VerifierConfig)
	if err != nil {
		return s.internalFailure(ctx, "build verifier", err, slog.String("verifier", in.Verifier))
	}
	secret, err := s.verifierSecret(ctx, in)
	if err != nil {
		return s.internalFailure(ctx, "open verifier secret", err, inboxAttrs(in)...)
	}
	// The decrypted secret is wiped when verification ends, however it ends.
	res, verr := func() (verify.Result, error) {
		defer clear(secret)
		return verifier.Verify(r, body, secret, s.opts.Now())
	}()

	// What was verified is what a verifier checked. The "none" verifier checks
	// nothing, and what it accepts is stored as unverified.
	verified, verifyReason := !res.Unauthenticated, ""
	if res.Unauthenticated {
		// Nothing was checked, so nothing the sender claimed is kept.
		res = verify.Result{Unauthenticated: true}
	}
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
				inboxAttrs(in, slog.String("reason", reason), slog.Int("size", len(body)))...)
			res := refuse(http.StatusUnauthorized, "verification_failed", "webhook verification failed")
			res.verifyFailure = verifyFailureOf(verr)
			return res
		}
	}

	// 5. A handshake (Slack's url_verification) is answered, not stored.
	if res.Challenge != nil {
		return ingestResult{status: http.StatusOK, challenge: res.Challenge}
	}

	// 6. Store it. Only once this returns is the delivery safe to acknowledge.
	//
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
	msg, created, err := s.append(ctx, in, &inbox.NewMessage{
		Method:      r.Method,
		PathSuffix:  r.PathValue("rest"),
		Query:       r.URL.RawQuery,
		Headers:     inbox.RedactHeaders(r.Header, redact...),
		ContentType: r.Header.Get("Content-Type"),
		Body:        body,
		Verified:    verified,
		VerifyError: verifyReason,
		DeliveryID:  res.DeliveryID,
		DedupeKey:   dedupeKey(res, body),
		EventType:   eventType,
		SourceIP:    clientIP(r, s.opts.TrustedProxies),
	})
	switch {
	case errors.Is(err, inbox.ErrNotFound): // deleted while we were verifying
		return refuse(http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, errUnverifiedFull):
		return refuse(http.StatusTooManyRequests, codeUnverifiedLimit, "this inbox holds as many unverified messages as it may; verified deliveries are still accepted")
	case errors.Is(err, inbox.ErrStorageFull):
		// Not stored, so not acknowledged: the sender keeps it and tries again.
		s.log.LogAttrs(ctx, slog.LevelError, "delivery refused: no room to store it", inboxAttrs(in, slog.String("err", err.Error()))...)
		return refuse(http.StatusServiceUnavailable, "storage_full", "the server has no room to store the delivery; retry later")
	case err != nil:
		return s.internalFailure(ctx, "append", err)
	}

	// 7. Committed. Tell the world, then acknowledge. Nothing after the commit
	// may turn into an error response: the sender would retry a delivery we
	// already hold.
	s.afterCommit(ctx, in, msg, created)
	s.log.LogAttrs(ctx, slog.LevelInfo, "delivery",
		inboxAttrs(in, slog.Int64("seq", msg.Seq), slog.Int("size", len(body)),
			slog.Bool("verified", verified), slog.Bool("duplicate", !created))...)
	return ingestResult{status: http.StatusOK, seq: msg.Seq, duplicate: !created, verified: msg.Verified}
}

// observe tells the Hooks' IngestObserver how a delivery to in ended. A panic in it
// is logged and swallowed: the outcome, and the response, are already decided.
func (s *Server) observe(ctx context.Context, in *inbox.Inbox, r *http.Request, res ingestResult) {
	defer func() {
		if rec := recover(); rec != nil {
			s.log.LogAttrs(ctx, slog.LevelError, "panic in ingest observer",
				inboxAttrs(in, slog.String("panic", fmt.Sprint(rec)))...)
		}
	}()
	s.observer.ObserveIngest(ctx, &inbox.IngestEvent{
		Inbox:         in,
		Outcome:       ingestOutcome(res),
		Status:        res.status,
		Size:          res.size,
		SourceIP:      clientIP(r, s.opts.TrustedProxies),
		Host:          requestHost(r),
		Seq:           res.seq,
		Verified:      res.verified,
		VerifyFailure: res.verifyFailure,
	})
}

// verifyFailureOf maps a verifier's failure kind onto the closed set an observer
// sees. A kind a custom verifier made up is dropped, not passed on.
func verifyFailureOf(err error) inbox.VerifyFailure {
	switch verify.FailureKindOf(err) {
	case verify.FailureMissing:
		return inbox.VerifyFailureMissing
	case verify.FailureMalformed:
		return inbox.VerifyFailureMalformed
	case verify.FailureMismatch:
		return inbox.VerifyFailureMismatch
	case verify.FailureStale:
		return inbox.VerifyFailureStale
	}
	return ""
}

// signatureHeaders names the stored request headers that carry in's signature, for
// verifiers that say (verify.SignatureHeaderer), or nil. Names only.
func (s *Server) signatureHeaders(in *inbox.Inbox) []string {
	v, err := s.opts.Verifiers.New(in.Verifier, in.VerifierConfig)
	if err != nil {
		return nil
	}
	sh, ok := v.(verify.SignatureHeaderer)
	if !ok {
		return nil
	}
	return slices.Clone(sh.SignatureHeaders())
}

// ingestOutcome names how a delivery ended for an IngestObserver.
func ingestOutcome(res ingestResult) inbox.IngestOutcome {
	if res.status == http.StatusOK {
		switch {
		case res.challenge != nil:
			return inbox.IngestHandshake
		case res.duplicate:
			return inbox.IngestDuplicate
		}
		return inbox.IngestStored
	}
	switch res.code {
	case "verification_failed":
		return inbox.IngestVerificationFailed
	case "rate_limited":
		return inbox.IngestRateLimited
	case "quota_exceeded":
		return inbox.IngestQuotaExceeded
	case "forbidden":
		return inbox.IngestForbidden
	case codeUnverifiedLimit:
		return inbox.IngestUnverifiedLimit
	case "storage_full":
		return inbox.IngestStorageFull
	case "overloaded":
		return inbox.IngestOverloaded
	case "payload_too_large":
		return inbox.IngestTooLarge
	case "bad_request":
		return inbox.IngestBadRequest
	case "not_found":
		return inbox.IngestNotFound
	}
	return inbox.IngestError
}

// errUnverifiedFull is append's answer for an unverified message the inbox has no
// room for under Options.MaxUnverified.
var errUnverifiedFull = errors.New("too many unverified messages")

// append stores m. An inbox holds only so many unverified messages, whoever sent
// them: a leaked ingest key must not be able to fill it, or under count-based
// retention push the genuine messages out. A message over the cap is refused with
// errUnverifiedFull, so it is not acknowledged, and nothing already stored is
// evicted to make room for it. Verified messages are never counted or refused, and
// take no lock. The count and the append happen under the inbox's own lock, so
// concurrent deliveries to it cannot overshoot the cap (across several servers on
// one database it is approximate), while deliveries to other inboxes go on. The
// lock covers only the two Store calls: the limiter is asked before it is taken,
// and the hooks run after it is given back.
func (s *Server) append(ctx context.Context, in *inbox.Inbox, m *inbox.NewMessage) (*inbox.Message, bool, error) {
	if m.Verified || s.unverified == nil {
		return s.opts.Store.Append(ctx, in.ID, m)
	}
	max := int64(s.opts.MaxUnverified)
	if s.limiter != nil {
		if v := s.limiter.MaxUnverified(ctx, in); v != 0 {
			max = int64(v)
		}
	}
	if max <= 0 {
		return s.opts.Store.Append(ctx, in.ID, m)
	}
	unlock, err := s.unverifiedLocks.lock(ctx, in.ID)
	if err != nil {
		return nil, false, err // the request ended while it waited
	}
	defer unlock()
	if n, err := s.unverified.CountUnverified(ctx, in.ID, max); err != nil {
		return nil, false, err
	} else if n >= max {
		// The cap that applied, which is the limiter's when it gave one. An inbox at its
		// cap refuses every unverified delivery, so the line is written once a minute per
		// inbox, with how many it left out; /metrics counts every one.
		if write, skipped := s.capLog.allow(in.ID); write {
			attrs := inboxAttrs(in, slog.Int64("max_unverified", max))
			if skipped > 0 {
				attrs = append(attrs, slog.Int64("refusals_not_logged", skipped))
			}
			s.log.LogAttrs(ctx, slog.LevelWarn, "delivery refused: too many unverified messages", attrs...)
		}
		return nil, false, errUnverifiedFull
	}
	return s.opts.Store.Append(ctx, in.ID, m)
}

// dedupeKey is what a repeated delivery is recognised by. A delivery id that the
// signature covers is enough. One taken from an unsigned header is not: a request
// captured in transit could be sent again with an id of the attacker's choosing,
// or with the id of a delivery that has not happened yet, and the genuine one
// would then be taken for a duplicate and dropped. So the key also holds a hash of
// the body, and only a repeat of the same body under the same id is a duplicate.
func dedupeKey(res verify.Result, body []byte) string {
	if res.DeliveryID == "" || res.IDSigned {
		return res.DeliveryID
	}
	sum := sha256.Sum256(body)
	return res.DeliveryID + "#sha256:" + hex.EncodeToString(sum[:])
}

// requestHost is the host a request was addressed to, for IngestRequest.Host: r.Host,
// lower-cased, with a trailing dot taken off the name and the port kept only if there
// is one. It never looks at X-Forwarded-Host or any other header.
func requestHost(r *http.Request) string {
	h := strings.ToLower(r.Host)
	name, port := h, ""
	if n, p, err := net.SplitHostPort(h); err == nil {
		name, port = n, p
		if strings.Contains(n, ":") {
			name = "[" + n + "]" // an IPv6 literal keeps its brackets
		}
	}
	name = strings.TrimSuffix(name, ".")
	if port != "" {
		return name + ":" + port
	}
	return name
}

// rejected is the answer to a delivery that BeforeIngest refused.
func (s *Server) rejected(ctx context.Context, in *inbox.Inbox, err error) ingestResult {
	var res ingestResult
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		// Answered as an unknown ingest key is, with nothing of the hook's: no
		// Retry-After, and nothing logged above debug (an unknown key logs nothing).
		s.log.LogAttrs(ctx, slog.LevelDebug, "delivery refused by hook as not found",
			inboxAttrs(in, slog.String("reason", err.Error()))...)
		return refuse(http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, inbox.ErrRateLimited):
		res = refuse(http.StatusTooManyRequests, "rate_limited", "too many requests")
	case errors.Is(err, inbox.ErrQuotaExceeded):
		res = refuse(http.StatusPaymentRequired, "quota_exceeded", "the quota for this inbox has been reached")
	case errors.Is(err, inbox.ErrForbidden):
		res = refuse(http.StatusForbidden, "forbidden", "this delivery is not allowed")
	default:
		return s.internalFailure(ctx, "before-ingest hook", err, inboxAttrs(in)...)
	}
	res.retryAfter = retryAfterOf(err)
	s.log.LogAttrs(ctx, slog.LevelInfo, "delivery refused by hook",
		inboxAttrs(in, slog.String("reason", err.Error()))...)
	return res
}

// afterCommit runs the post-commit notifications. A panic in an embedder's
// code is logged and swallowed, because by now the message is stored.
func (s *Server) afterCommit(ctx context.Context, in *inbox.Inbox, msg *inbox.Message, created bool) {
	defer func() {
		if rec := recover(); rec != nil {
			s.log.LogAttrs(ctx, slog.LevelError, "panic after commit",
				inboxAttrs(in, slog.Int64("seq", msg.Seq), slog.String("panic", fmt.Sprint(rec)))...)
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
// has none. The caller should clear the result when done. ctx is the delivery's,
// so a SecretBox that takes one stops when the sender goes away; the box is given
// it scoped to the inbox's tenant (see inbox.ContextSecretBox), while the Store is
// still asked unscoped.
func (s *Server) verifierSecret(ctx context.Context, in *inbox.Inbox) ([]byte, error) {
	if len(in.VerifierSecret) == 0 {
		return nil, nil
	}
	if s.opts.SecretBox == nil {
		return nil, errors.New("the inbox has a verifier secret but the server has no SecretBox")
	}
	secret, err := inbox.OpenWith(inbox.WithTenant(ctx, in.Tenant), s.opts.SecretBox, in.VerifierSecret, inbox.VerifierSecretContext(in.UID))
	if err != nil {
		return nil, fmt.Errorf("the stored verifier secret cannot be opened (is the master key the one it was sealed under, and is the key service, if any, working for this inbox's tenant?): %w", err)
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
