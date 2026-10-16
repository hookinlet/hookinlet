// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

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
type Authenticator struct {
	store     inbox.Store
	adminHash []byte // nil when no admin token is configured
}

var _ inbox.Authenticator = (*Authenticator)(nil)

// NewAuthenticator returns an Authenticator over store. adminToken may be empty,
// in which case no request authenticates as the admin.
func NewAuthenticator(store inbox.Store, adminToken string) *Authenticator {
	a := &Authenticator{store: store}
	if adminToken != "" {
		a.adminHash = Hash(adminToken)
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
	if tk.AllInboxes {
		return Reader{}, nil
	}
	return Agent{ids: tk.InboxIDs}, nil
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
