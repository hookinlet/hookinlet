// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import (
	"errors"
	"fmt"
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

// Invalidf returns an error that wraps ErrInvalid with a formatted reason.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
