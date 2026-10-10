// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import (
	"context"
	"net/http"
	"time"
)

// Store persists inboxes and their messages. All methods are safe for
// concurrent use and honor context cancellation.
//
// # Sequence numbers
//
// Each inbox has its own sequence counter. Append assigns the next number
// (1, 2, 3, ...) inside the same atomic step that makes the message visible,
// so a message can never become visible with a Seq lower than one a reader has
// already observed. Seqs are therefore assigned in commit order. This is what
// makes a Seq a safe cursor: a reader that has seen Seq k and later queries
// After(k) sees every message that commits afterward, exactly once, and never
// sees a message with a Seq at or below k appear late. Implementations on
// databases that hand out ids at insert time but commit out of order (such as
// Postgres sequences) must serialize appends per inbox, for example with a row
// lock on a per-inbox counter. The conformance suite tests this property under
// concurrency.
//
// Seqs are never reused. Retention removes old messages but leaves LastSeq
// untouched, so the next Append continues the count.
//
// # Errors
//
// Missing inboxes or messages yield ErrNotFound, uniqueness violations
// ErrConflict, and invalid arguments ErrInvalid, each possibly wrapped.
type Store interface {
	// ResolveIngestKey maps the key in an ingest URL (/in/{key}) to its inbox.
	// The open-source implementation treats the key as the slug. It returns
	// ErrNotFound for an unknown key and may return a disabled inbox; callers
	// must check Inbox.Disabled.
	ResolveIngestKey(ctx context.Context, key string) (*Inbox, error)

	// Append stores m in the inbox and returns the stored message. If
	// m.DeliveryID is non-empty and the inbox already holds a message with that
	// delivery id, Append returns the existing message with created == false,
	// stores nothing, and does not consume a sequence number. Otherwise created
	// is true. Append drops AlwaysRedactedHeaders from m.Headers before storing
	// and returns ErrNotFound if the inbox does not exist. It does not enforce
	// body-size limits; the caller does.
	Append(ctx context.Context, inboxID ID, m *NewMessage) (msg *Message, created bool, err error)

	// Query returns messages per q (see Query for ordering and paging).
	Query(ctx context.Context, inboxID ID, q Query) (*Page, error)

	// Get returns one message including its body. A seq that does not exist in
	// this inbox, including one that exists only in another inbox, is
	// ErrNotFound.
	Get(ctx context.Context, inboxID ID, seq int64) (*Message, error)

	// LatestSeq returns the inbox's newest sequence number, 0 if it has never
	// received a message.
	LatestSeq(ctx context.Context, inboxID ID) (int64, error)

	// Stats returns message counts for the inbox.
	Stats(ctx context.Context, inboxID ID) (*InboxStats, error)

	// CreateInbox validates in (after ApplyDefaults), assigns ID, CreatedAt,
	// and UpdatedAt, sets LastSeq to 0, and stores it, updating *in. It returns
	// ErrConflict if the slug is taken.
	CreateInbox(ctx context.Context, in *Inbox) error

	// UpdateInbox replaces the mutable fields of an existing inbox: Name,
	// Verifier, VerifierConfig, VerifierSecret, OnVerifyFail, AgentTokenHash,
	// MaxBodyBytes, RetentionDays, RetentionMaxCount, EventTypePointer, and
	// Disabled. ID, Slug, LastSeq, and CreatedAt are never changed, and an
	// update never disturbs sequence assignment for concurrent Appends. It
	// refreshes UpdatedAt and *in, and returns ErrNotFound if the inbox is gone.
	UpdateInbox(ctx context.Context, in *Inbox) error

	// DeleteInbox removes the inbox and all of its messages.
	DeleteInbox(ctx context.Context, id ID) error

	GetInbox(ctx context.Context, id ID) (*Inbox, error)
	GetInboxBySlug(ctx context.Context, slug string) (*Inbox, error)
	ListInboxes(ctx context.Context, p ListParams) ([]*Inbox, error)

	// Prune applies every inbox's retention policy as of now: it deletes
	// messages older than RetentionDays and, beyond RetentionMaxCount, the
	// oldest messages. It returns the number of messages deleted.
	Prune(ctx context.Context, now time.Time) (deleted int64, err error)

	// Close releases resources. The Store must not be used afterward.
	Close() error
}

// Notifier tells interested parties that an inbox may have new messages. It
// carries no payload: a subscriber wakes up and reads from the Store at its own
// cursor. Replay, reconnects, and slow consumers therefore share one code
// path, and a missed signal costs latency, never data.
type Notifier interface {
	// Publish signals that the inbox now has a message at seq. It never blocks
	// the caller and must be called only after the message is committed.
	// Delivery is at-most-once; implementations may drop signals.
	Publish(ctx context.Context, inboxID ID, seq int64)

	// Subscribe returns a channel that receives a signal whenever the inbox may
	// have new messages, and a function that cancels the subscription. Signals
	// coalesce: if several arrive before the receiver reads, it sees one. The
	// channel is never closed. cancel is idempotent, and cancelling ctx also
	// ends the subscription. Both release all resources held for it.
	Subscribe(ctx context.Context, inboxID ID) (signal <-chan struct{}, cancel func())
}

// Authenticator resolves a request's credentials to a Principal.
type Authenticator interface {
	// Authenticate returns the caller's Principal, or an error wrapping
	// ErrUnauthenticated if the credentials are missing or invalid.
	Authenticate(r *http.Request) (Principal, error)
}

// Principal is an authenticated caller and what it may do.
type Principal interface {
	// CanReadInbox reports whether the caller may read in's messages. Callers
	// answer a false result with 404, not 403, so a credential for one inbox
	// never reveals whether another exists.
	CanReadInbox(in *Inbox) bool
	// CanAdmin reports whether the caller may use the admin API.
	CanAdmin() bool
}

// Hooks lets an embedder observe and gate ingest. The open-source default does
// nothing.
type Hooks interface {
	// BeforeIngest may reject a delivery before it is verified or stored by
	// returning an error wrapping ErrRateLimited (429), ErrQuotaExceeded (402),
	// or ErrForbidden (403). size is the body size in bytes. Any other error is
	// treated as an internal failure.
	BeforeIngest(ctx context.Context, in *Inbox, size int64) error

	// AfterIngest runs after a message is committed. It must not block.
	AfterIngest(ctx context.Context, in *Inbox, m *Message)
}

// SecretBox seals secrets for storage at rest.
type SecretBox interface {
	// Seal encrypts plaintext. Sealing the same plaintext twice yields
	// different ciphertexts.
	Seal(plaintext []byte) ([]byte, error)
	// Open decrypts ciphertext produced by Seal. It returns an error if the
	// ciphertext was tampered with, truncated, or sealed under another key.
	Open(ciphertext []byte) ([]byte, error)
}
