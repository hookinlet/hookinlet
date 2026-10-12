// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Store persists inboxes and their messages. All methods are safe for
// concurrent use and honor context cancellation: a cancelled call returns an
// error wrapping the context's error and leaves no partial effect.
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
// # Visibility
//
// Once Append returns, every later read by any caller, on any node, observes
// the message. A notifier wake-up followed by a read must never find the
// message missing, so implementations that read from replicas must read their
// own writes.
//
// # Time
//
// The Store owns the clock. ReceivedAt, CreatedAt, and UpdatedAt are UTC,
// truncated to TimePrecision (see NormalizeTime), and what a write returns is
// exactly what later reads return. Within one inbox, ReceivedAt never
// decreases as Seq increases, even if the system clock steps backwards:
// Append uses the later of the clock and the previous message's ReceivedAt.
//
// # Errors
//
// Missing inboxes or messages yield ErrNotFound (including Query, LatestSeq,
// and Stats on an inbox that does not exist), uniqueness violations
// ErrConflict, and invalid arguments ErrInvalid, each possibly wrapped.
type Store interface {
	// ResolveIngestKey maps the key in an ingest URL (/in/{key}) to its inbox.
	// A key is the inbox's IngestKey. It returns
	// ErrNotFound for an unknown key and may return a disabled inbox; callers
	// must check Inbox.Disabled. The server calls it on every delivery and
	// does not cache the result: an implementation that needs a cache owns it,
	// including cross-node invalidation.
	ResolveIngestKey(ctx context.Context, key string) (*Inbox, error)

	// Append stores m in the inbox and returns the stored message, assigning
	// its Seq and ReceivedAt. It drops AlwaysRedactedHeaders from m.Headers,
	// returns ErrNotFound if the inbox does not exist, and does not enforce
	// body-size limits (the caller does).
	//
	// Deduplication: if m.Verified and the dedupe key (m.DedupeKey, or
	// m.DeliveryID when that is empty) is non-empty and the inbox
	// already holds a verified message with that key, Append stores
	// nothing, consumes no sequence number, and returns the existing message
	// with created == false. Otherwise created is true. Unverified messages
	// are never deduplicated and never match: a forged delivery that claims a
	// real delivery id must not suppress the genuine one. When several Appends
	// with the same verified key race, exactly one gets created == true
	// and the rest get that message back with created == false; none returns
	// ErrConflict. Once a message is pruned, its key may be stored
	// again.
	Append(ctx context.Context, inboxID ID, m *NewMessage) (msg *Message, created bool, err error)

	// Query returns messages per q (see Query for ordering and paging).
	//
	// Two guarantees let a caller follow an inbox without rescanning it, and an
	// implementation must keep both:
	//
	//   - Exactness: the result holds exactly min(q.EffectiveLimit(), matches)
	//     messages. A page shorter than the limit therefore means nothing more
	//     matches, not that the Store stopped early for a size or time reason.
	//   - Snapshot: Page.LatestSeq is no newer than the state the messages were
	//     read from (one transaction, or read first). Every message that matches
	//     and has a Seq at or below LatestSeq is in the result or beyond a full
	//     page. A LatestSeq read after the messages could name a Seq that
	//     committed in between, and a caller who skipped ahead to it would never
	//     see that message.
	//
	// Together they mean that when an ascending read comes back short, the
	// caller may continue after max(last returned Seq, LatestSeq).
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

	// CreateInbox applies defaults, validates in (including that it has an
	// IngestKey, which a Store does not make up), assigns ID, CreatedAt,
	// UpdatedAt, and Version 1, sets LastSeq to 0, and stores it, updating
	// *in. It returns ErrConflict if the slug or the ingest key is already used
	// by another inbox. A new inbox has no tokens.
	CreateInbox(ctx context.Context, in *Inbox) error

	// UpdateInbox replaces the mutable fields of an existing inbox: Name,
	// Verifier, VerifierConfig, VerifierSecret, OnVerifyFail, IngestKey,
	// MaxBodyBytes, RetentionDays, RetentionMaxCount, EventTypePointer, and
	// Disabled. ID, Slug, LastSeq, and CreatedAt are never changed, and an
	// update never disturbs sequence assignment for concurrent Appends.
	//
	// It is a compare-and-swap on Version: if in.Version is not the stored
	// version, nothing changes and the error wraps ErrConflict, so two admins
	// cannot silently overwrite each other. Callers re-read and retry. On
	// success the stored Version is incremented and *in is refreshed. It returns
	// ErrNotFound if the inbox is gone.
	UpdateInbox(ctx context.Context, in *Inbox) error

	// DeleteInbox removes the inbox and all of its messages. A Store may do it in
	// steps so that a large inbox does not hold up everyone else's writes: it
	// must first make the inbox unreachable (ingest answers not found, and the
	// tokens scoped to it stop reading it: a token left with no inbox is
	// deleted, including one issued while the delete was running), and if it is interrupted it leaves the inbox in that state, so that
	// calling DeleteInbox again finishes the job.
	DeleteInbox(ctx context.Context, id ID) error

	// GetInbox returns the inbox with that ID, or ErrNotFound.
	GetInbox(ctx context.Context, id ID) (*Inbox, error)
	// GetInboxBySlug returns the inbox with that slug, or ErrNotFound.
	GetInboxBySlug(ctx context.Context, slug string) (*Inbox, error)
	// ListInboxes pages through the inboxes in ID order (see ListParams).
	ListInboxes(ctx context.Context, p ListParams) ([]*Inbox, error)

	// CreateToken validates t, assigns ID and CreatedAt, and stores it, updating
	// *t. Every inbox in t.InboxIDs must exist (ErrNotFound otherwise, and
	// nothing is stored), and the hash must not belong to another token
	// (ErrConflict).
	CreateToken(ctx context.Context, t *Token) error
	// GetTokenByHash finds the token with this hash, or returns ErrNotFound. It
	// must be an indexed lookup, not a scan: the server calls it on every
	// authenticated request.
	GetTokenByHash(ctx context.Context, hash []byte) (*Token, error)
	// ListTokens pages through tokens (see TokenListParams).
	ListTokens(ctx context.Context, p TokenListParams) ([]*Token, error)
	// DeleteToken removes a token, which stops working at once. It returns
	// ErrNotFound if there is none.
	DeleteToken(ctx context.Context, id TokenID) error

	// Prune applies every inbox's retention policy as of now. A message is
	// deleted if either rule selects it: it is strictly older than
	// RetentionDays*24h before now, or it is not among the newest
	// RetentionMaxCount messages by Seq. Because ReceivedAt never decreases
	// with Seq, what is deleted is always a prefix of an inbox's messages.
	// Prune works in bounded batches. It returns the number of messages
	// deleted, and if ctx is cancelled partway, the count so far together
	// with the error. Pruning is idempotent.
	Prune(ctx context.Context, now time.Time) (deleted int64, err error)

	// Ping verifies the Store is reachable and usable, for readiness checks.
	Ping(ctx context.Context) error

	// Close releases resources. The Store must not be used afterward.
	Close() error
}

// StoreStatus is what a StatusReporter says about its storage.
type StoreStatus struct {
	// SchemaVersion is the newest applied migration.
	SchemaVersion int
	// DatabaseBytes is the size of the data, not counting its log.
	DatabaseBytes int64
	// FreeBytes is the room left where the data lives. It is meaningful only when
	// FreeKnown is true.
	FreeBytes uint64
	FreeKnown bool
}

// StatusReporter is an optional capability of a Store: facts about its storage that
// the admin console shows. A backend that cannot report them leaves it out, and the
// status endpoint omits what it does not know.
type StatusReporter interface {
	Status(ctx context.Context) (StoreStatus, error)
}

// Backupper is an optional capability of a Store: a consistent copy of all its
// data that can be saved elsewhere. It is optional because not every backend
// can offer one (a hosted database is backed up by its operator). Callers find
// it with a type assertion and answer 501 when it is absent; this is the one
// documented exception to the rule that handlers reach the Store only through its interface.
type Backupper interface {
	// Backup returns a snapshot of the Store as of a single moment: everything
	// committed before the call is in it, and nothing in it is half-applied.
	// size is the snapshot's length in bytes, and the reader yields exactly that
	// many; or -1 when the Store streams the snapshot and cannot know its length
	// until it is read, in which case the reader ends it with io.EOF only when it
	// is complete, and returns an error otherwise. The caller must Close the reader, which releases whatever the
	// snapshot holds (a temporary file, for instance). Appends and reads continue
	// while a backup is taken and while it is read.
	//
	// The snapshot is in the Store's own format. For secrets it holds only what
	// the Store holds: verifier secrets still sealed, and token hashes.
	Backup(ctx context.Context) (snapshot io.ReadCloser, size int64, err error)
}

// Notifier tells interested parties that an inbox may have new messages. It
// carries no payload: a subscriber wakes up and reads from the Store at its own
// cursor. Replay, reconnects, and slow consumers therefore share one code
// path, and a missed signal costs latency, never data.
type Notifier interface {
	// Publish signals that the inbox now has a message at seq. It never blocks
	// the caller and must be called only after the message is committed.
	//
	// Every subscription that exists when Publish begins, that is, one whose
	// Subscribe has returned, must be signalled. Consumers depend on this: they
	// subscribe first and read second, so a message committed after the read is
	// announced to a subscription that is already there.
	//
	// ctx belongs to the request that stored the message and may be cancelled by
	// the time Publish runs. An implementation must still deliver the signal.
	//
	// An in-process notifier must not drop signals. A distributed notifier may
	// (delivery is at-most-once), and must then be deployed with a positive
	// Options.SSERecheckInterval (HOOKINLET_SSE_RECHECK_INTERVAL) so subscribers re-read the Store periodically.
	Publish(ctx context.Context, inboxID ID, seq int64)

	// Subscribe returns a channel that receives a signal whenever the inbox may
	// have new messages, and a function that cancels the subscription. Signals
	// coalesce: if several arrive before the receiver reads, it sees one. The
	// channel is never closed. cancel is idempotent, and cancelling ctx also
	// ends the subscription. Both release all resources held for it. An
	// implementation that cannot subscribe (for example its backend is down)
	// returns a channel that never signals and logs the failure, so callers
	// degrade to periodic re-reads.
	Subscribe(ctx context.Context, inboxID ID) (signal <-chan struct{}, cancel func())
}

// Authenticator resolves a request's credentials to a Principal.
//
// Authenticate runs before the server knows whether the caller is allowed to
// see anything, on requests from anyone, so an implementation that consults a
// database should be paired with rate limiting in front of it.
type Authenticator interface {
	// Authenticate returns the caller's Principal, or an error wrapping
	// ErrUnauthenticated if the credentials are missing or invalid. Every such
	// failure must look the same to the caller: no error text, status, or
	// timing that says whether a token once existed, expired, or belongs to a
	// different inbox.
	//
	// Any other error is taken to mean the credential could not be checked (the
	// Store is down) and becomes a server error, not a 401. An implementation
	// must therefore wrap ErrUnauthenticated for an unknown token and must not
	// return some other error for it.
	Authenticate(r *http.Request) (Principal, error)
}

// Principal is an authenticated caller and what it may do.
type Principal interface {
	// CanReadInbox reports whether the caller may read in's messages. Callers
	// answer a false result with 404, not 403, so a credential for one inbox
	// never reveals whether another exists. A disabled inbox stays readable.
	//
	// The decision must follow the inbox's ID, which never changes, and not its
	// slug or name, which a different inbox can carry. It must not depend on
	// Inbox.Disabled. A nil inbox is never readable.
	CanReadInbox(in *Inbox) bool
	// CanAdmin reports whether the caller may use the admin API.
	CanAdmin() bool
}

// ScopedPrincipal is an optional capability of a Principal that can read a known,
// finite list of inboxes (an agent token scoped to some of them). Anything that
// needs to list the inboxes a caller can read uses it to go straight to those,
// instead of walking every inbox and asking CanReadInbox, which would cost time in
// proportion to the number of inboxes and could not tell a caller that its list
// was complete without telling it how many others exist.
//
// InboxIDs must be exactly the IDs of the inboxes CanReadInbox accepts. A
// Principal that can read an unbounded set, or that can use the admin API, must not
// implement it.
type ScopedPrincipal interface {
	Principal
	// InboxIDs lists the inboxes the principal is limited to.
	InboxIDs() []ID
}

// IngestRequest describes a delivery that has been received and not yet verified,
// for Hooks.BeforeIngest. Nothing in it has been checked: every field except Inbox
// came from whoever sent the request. A hook must not modify it.
type IngestRequest struct {
	// Inbox is the inbox the delivery is for.
	Inbox *Inbox
	// Size is the body size in bytes.
	Size int64
	// SourceIP is where the delivery came from, as the server records it: the
	// peer's address, or the client's behind a trusted proxy. It may be empty.
	SourceIP string
	// Method is POST or PUT, and PathSuffix is what followed the inbox key in the
	// URL (without the leading slash).
	Method, PathSuffix string
	// Headers are the request headers as received. They are shared with the
	// request: read them, do not change them.
	Headers http.Header
}

// Hooks lets an embedder observe and gate ingest. The open-source default does
// nothing.
type Hooks interface {
	// BeforeIngest may reject a delivery before it is verified or stored by
	// returning an error wrapping ErrRateLimited (429), ErrQuotaExceeded (402),
	// or ErrForbidden (403). Any other error is treated as an internal failure.
	// The request is what is known of the delivery at that point; it grows by
	// adding fields, which is why it is a struct.
	BeforeIngest(ctx context.Context, req *IngestRequest) error

	// AfterIngest runs after a delivery is committed or recognized as a
	// duplicate. created is false for a duplicate, so a metering hook can
	// avoid double counting. It must not block. The ctx may be cancelled as
	// soon as the HTTP response is sent; work that outlives the call must use
	// context.WithoutCancel.
	AfterIngest(ctx context.Context, in *Inbox, m *Message, created bool)
}

// SecretBox seals secrets for storage at rest.
type SecretBox interface {
	// Seal encrypts plaintext and binds it to context: the same context must be
	// given to Open. The caller chooses what the value belongs to (for a verifier
	// secret, VerifierSecretContext(slug)), so that a sealed value copied onto another row does not
	// open there. Sealing the same plaintext twice yields different ciphertexts.
	Seal(plaintext, context []byte) ([]byte, error)
	// Open decrypts ciphertext produced by Seal, given the context it was sealed
	// with. It returns an error if the ciphertext was tampered with, truncated,
	// sealed under another key, or sealed for another context.
	Open(ciphertext, context []byte) ([]byte, error)
}
