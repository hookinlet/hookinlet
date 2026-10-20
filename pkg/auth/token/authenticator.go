// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package token

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// maxTokenLen bounds the credential we are willing to hash. Real tokens are 47
// characters; anything much longer is not one.
const maxTokenLen = 512

// Authenticator is the default inbox.Authenticator. It recognizes two kinds of
// bearer token:
//
//   - the admin token, configured at startup, which can read every inbox and use
//     the admin API;
//   - a token stored with the inboxes, which reads the inboxes it is scoped to
//     (or all of them) and never administers.
//
// Stored tokens are looked up by their hash, so the database never holds a token
// that could be replayed.
//
// A stored token may carry an expiry (inbox.Token.ExpiresAt). The Store does not
// return an expired token, and the Authenticator checks again, so that a Store
// that does not enforce expiry still cannot let one through. An expired token is
// refused exactly as an unknown one is.
//
// When the Store can record it (inbox.TokenUseRecorder), the Authenticator notes
// when a token was last used, at most once per interval per token and without
// holding up or failing the request.
type Authenticator struct {
	store     inbox.Store
	adminHash []byte // nil when no admin token is configured
	now       func() time.Time
	log       *slog.Logger
	recorder  inbox.TokenUseRecorder // nil when the Store records no last use
	interval  time.Duration          // between recordings of one token's use; <= 0 means never

	mu       sync.Mutex
	recorded map[inbox.TokenID]time.Time // when each token's use was last handed to the recorder
	inflight atomic.Int32
}

var _ inbox.Authenticator = (*Authenticator)(nil)

// DefaultLastUsedInterval is how often at most one token's last use is
// recorded.
const DefaultLastUsedInterval = 5 * time.Minute

// maxTouchesInFlight bounds the recordings running at once. A recording is
// skipped, and tried again on the token's next use, when the Store is too slow
// to keep up, so that a stuck Store cannot pile up goroutines.
const maxTouchesInFlight = 8

// touchTimeout bounds one recording.
const touchTimeout = 5 * time.Second

// maxRecorded bounds the memory of when tokens were recorded; past it, entries
// that are due again anyway are dropped.
const maxRecorded = 4096

// Option configures an Authenticator.
type Option func(*Authenticator)

// WithClock sets the clock used to decide that a token has expired and when its
// use was last recorded. Pass the server's Options.Now so that they agree about
// the time. The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(a *Authenticator) {
		if now != nil {
			a.now = now
		}
	}
}

// WithLastUsedInterval sets how often at most one token's last use is recorded.
// Zero means DefaultLastUsedInterval; a negative value turns recording off.
func WithLastUsedInterval(d time.Duration) Option {
	return func(a *Authenticator) { a.interval = d }
}

// WithLogger sets where a failed recording of a token's use is reported (once
// per attempt, which the interval keeps rare). The default discards them.
func WithLogger(l *slog.Logger) Option {
	return func(a *Authenticator) {
		if l != nil {
			a.log = l
		}
	}
}

// NewAuthenticator returns an Authenticator over store. adminToken may be empty,
// in which case no request authenticates as the admin.
func NewAuthenticator(store inbox.Store, adminToken string, opts ...Option) *Authenticator {
	a := &Authenticator{
		store: store, now: time.Now, interval: DefaultLastUsedInterval,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), recorded: map[inbox.TokenID]time.Time{},
	}
	if adminToken != "" {
		a.adminHash = Hash(adminToken)
	}
	for _, o := range opts {
		o(a)
	}
	if a.interval == 0 {
		a.interval = DefaultLastUsedInterval
	}
	if rec, ok := store.(inbox.TokenUseRecorder); ok && a.interval > 0 {
		a.recorder = rec
	}
	return a
}

// Admin is the Principal for the admin token.
type Admin struct{}

// CanReadInbox implements inbox.Principal: the admin reads every inbox.
func (Admin) CanReadInbox(in *inbox.Inbox) bool { return in != nil }

// CanAdmin implements inbox.Principal.
func (Admin) CanAdmin() bool { return true }

// Agent is the Principal for a token scoped to particular inboxes.
type Agent struct{ ids []inbox.ID }

// InboxIDs returns the inboxes this principal can read.
func (a Agent) InboxIDs(context.Context) ([]inbox.ID, error) { return slices.Clone(a.ids), nil }

// CanReadInbox implements inbox.Principal: an agent reads only the inboxes its
// token names.
func (a Agent) CanReadInbox(in *inbox.Inbox) bool { return in != nil && slices.Contains(a.ids, in.ID) }

// CanAdmin implements inbox.Principal: an agent is never an admin.
func (Agent) CanAdmin() bool { return false }

// Reader is the Principal for a token that reads every inbox but cannot
// administer.
type Reader struct{}

// CanReadInbox implements inbox.Principal.
func (Reader) CanReadInbox(in *inbox.Inbox) bool { return in != nil }

// CanAdmin implements inbox.Principal.
func (Reader) CanAdmin() bool { return false }

// errUnauthenticated is what every authentication failure looks like. It says
// nothing about why, so it cannot tell a caller whether a token once existed.
var errUnauthenticated = fmt.Errorf("%w: a valid bearer token is required", inbox.ErrUnauthenticated)

// Authenticate implements inbox.Authenticator. It accepts exactly one
// "Authorization: Bearer <token>" header; tokens in URLs are not supported,
// because URLs end up in logs.
//
// A failure to authenticate wraps inbox.ErrUnauthenticated. Any other error
// means the credential could not be checked (the Store failed) and is not a
// statement about the credential.
func (a *Authenticator) Authenticate(r *http.Request) (inbox.Principal, error) {
	tok, ok := bearerToken(r)
	if !ok {
		return nil, errUnauthenticated
	}
	if a.adminHash != nil && Equal(tok, a.adminHash) {
		return Admin{}, nil
	}
	// Every token we issue starts with Prefix, so anything else cannot be an
	// agent token and does not need a database lookup.
	if !strings.HasPrefix(tok, Prefix) {
		return nil, errUnauthenticated
	}
	tk, err := a.store.GetTokenByHash(r.Context(), Hash(tok))
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		return nil, errUnauthenticated
	case err != nil:
		return nil, fmt.Errorf("token: look up agent token: %w", err)
	}
	now := a.now()
	if tk.ExpiresAt != nil && !now.Before(*tk.ExpiresAt) {
		return nil, errUnauthenticated // the same value as an unknown token
	}
	a.recordUse(tk.ID, now)
	if tk.AllInboxes {
		return Reader{}, nil
	}
	return Agent{ids: tk.InboxIDs}, nil
}

// recordUse hands a token's use to the Store's recorder, if it has one and the
// token has not been recorded within the interval. The write happens in the
// background, under a context of its own that the request's end cannot cancel,
// and its failure is only logged: noting a use must never fail or delay the
// request, and on SQLite's single writer it must not happen on every one.
func (a *Authenticator) recordUse(id inbox.TokenID, now time.Time) {
	if a.recorder == nil {
		return
	}
	a.mu.Lock()
	if last, ok := a.recorded[id]; ok && now.Sub(last) < a.interval && !now.Before(last) {
		a.mu.Unlock()
		return
	}
	if a.inflight.Add(1) > maxTouchesInFlight {
		a.inflight.Add(-1)
		a.mu.Unlock()
		return
	}
	if len(a.recorded) >= maxRecorded {
		for k, v := range a.recorded {
			if now.Sub(v) >= a.interval {
				delete(a.recorded, k)
			}
		}
	}
	a.recorded[id] = now
	a.mu.Unlock()

	go func() {
		defer a.inflight.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
		defer cancel()
		if err := a.recorder.TouchToken(ctx, id, now); err != nil && !errors.Is(err, inbox.ErrNotFound) {
			a.log.LogAttrs(ctx, slog.LevelWarn, "could not record a token's last use",
				slog.Int64("token_id", int64(id)), slog.String("err", err.Error()))
		}
	}()
}

// bearerToken extracts the token from a request with exactly one Authorization
// header of the form "Bearer <token>". The scheme is case-insensitive
// (RFC 7235); the token must be non-empty, contain no whitespace, and be of a
// sane length.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(values[0], " ")
	if !ok || strings.ToLower(scheme) != "bearer" {
		return "", false
	}
	if tok == "" || len(tok) > maxTokenLen || strings.ContainsAny(tok, " \t\r\n") {
		return "", false
	}
	return tok, true
}
