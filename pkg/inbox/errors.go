// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors. Implementations wrap them with detail (fmt.Errorf with %w);
// callers test with errors.Is.
var (
	// ErrNotFound means the inbox or message does not exist (or, for a Store
	// scoped to a tenant, is not visible to the caller).
	ErrNotFound = errors.New("not found")
	// ErrConflict means a uniqueness constraint failed, such as a slug that is
	// already taken.
	ErrConflict = errors.New("conflict")
	// ErrInvalid means the input failed validation. The wrapped message says why
	// and is safe to show to the caller.
	ErrInvalid = errors.New("invalid input")
	// ErrEpochChanged means a cursor was issued for an earlier life of an inbox
	// (see Inbox.Epoch). The server answers it with HTTP 409; a Store never
	// returns it.
	ErrEpochChanged = errors.New("epoch changed")
	// ErrStorageFull means the Store has no room to write. A Store returns it from
	// a write that failed for lack of space, and from Ping while it is short of it.
	// The server answers a delivery with HTTP 503, since the sender should try again
	// when there is room, and readiness fails.
	ErrStorageFull = errors.New("storage full")
	// ErrUnauthenticated is returned by Authenticator.Authenticate when the
	// credentials are missing or wrong. It maps to HTTP 401.
	ErrUnauthenticated = errors.New("unauthenticated")

	// The errors below are returned by Hooks.BeforeIngest to reject a delivery.

	// ErrRateLimited rejects with HTTP 429.
	ErrRateLimited = errors.New("rate limited")
	// ErrQuotaExceeded rejects with HTTP 402.
	ErrQuotaExceeded = errors.New("quota exceeded")
	// ErrForbidden rejects with HTTP 403.
	ErrForbidden = errors.New("forbidden")
)

// RetryAfterer is an optional capability of an error a hook refuses with (from
// Hooks.BeforeIngest or Admitter.Admit, wrapping ErrRateLimited, ErrQuotaExceeded or
// ErrForbidden), found with errors.As: it says when the caller may try again. The
// server then sends Retry-After with the 429, 402 or 403, in whole seconds rounded up,
// at least 1 and at most a day; a duration that is not positive sends none. Over MCP,
// where the answer is a tool error, the text says "retry after N s". RefuseAfter makes
// such an error.
type RetryAfterer interface {
	RetryAfter() time.Duration
}

// RefuseAfter returns err carrying retryAfter, so that the caller is told when to try
// again (see RetryAfterer). err is a refusal, such as ErrRateLimited or an error that
// wraps it: what errors.Is and Error report is err's own. RefuseAfter(nil, d) is nil.
func RefuseAfter(err error, retryAfter time.Duration) error {
	if err == nil {
		return nil
	}
	return &retryAfterError{err: err, after: retryAfter}
}

type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string             { return e.err.Error() }
func (e *retryAfterError) Unwrap() error             { return e.err }
func (e *retryAfterError) RetryAfter() time.Duration { return e.after }

// Invalidf returns an error that wraps ErrInvalid with a formatted reason.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
