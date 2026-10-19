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
// auto-increment sequences) must serialize appends per inbox, for example with a
// row lock on a per-inbox counter. The conformance suite tests this property under
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
// # Tenants
//
// A Store may hold the inboxes of several independent parties, each under its own
// tenant key (Inbox.Tenant). The caller says which by scoping the context with
// WithTenant; the server does it for every request whose Principal is a
// TenantPrincipal. What a Store does depends on whether its context is scoped:
//
//   - Scoped to tenant T: every method acts only within T. An inbox of another
//     tenant is ErrNotFound to every method that takes an inbox, a slug or an
//     ingest key, whatever the ID, and ListInboxes does not show it. CreateInbox
//     makes the inbox in T (an Inbox.Tenant that is neither empty nor T is
//     ErrInvalid). Two tenants may use the same slug. CreateToken refuses an
//     InboxIDs entry that is not an inbox of T (ErrNotFound); the token methods
//     are otherwise not scoped (see below).
//   - Not scoped: the operator's view, used for deliveries, the retention sweep,
//     the command line and anything else that is not on behalf of a tenant. Methods
//     that take an ID or an ingest key, and ListInboxes, reach every inbox;
//     CreateInbox makes the inbox in Inbox.Tenant. GetInboxBySlug looks in the
//     default tenant (the empty key), since a slug names an inbox only within one.
//
// Ingest keys and UIDs are unique across all tenants, because an ingest key alone
// finds the inbox a delivery is for. Installations that never scope a context see
// no difference: every inbox is in the default tenant.
//
// Tokens are the operator's, not a tenant's: a Token has no tenant, and
// GetTokenByHash, ListTokens (including TokenListParams.InboxID) and DeleteToken
// act on every token whether or not the context is scoped, returning and
// deleting exactly what they would unscoped. GetTokenByHash has to: the server
// calls it to authenticate a request, before it knows the tenant. The one check
// a scope adds is CreateToken's, above; an AllInboxes token is not limited to T,
// since a Token records no tenant, and reads every inbox in the Store. An
// embedder that gives each tenant's callers credentials of their own keeps them
// in its own Authenticator, which returns a TenantPrincipal, rather than in
// these methods. The Tenants and TokensAreNotScopedByTenant cases of StoreSuite
// check this.
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
	// *in. It keeps in.UID if the caller gave one and makes one (NewUID) if not.
	// It returns ErrConflict if the slug, the ingest key, or the UID is already
	// used by another inbox. A new inbox has no tokens.
	CreateInbox(ctx context.Context, in *Inbox) error

	// UpdateInbox replaces the mutable fields of an existing inbox: Name,
	// Verifier, VerifierConfig, VerifierSecret, OnVerifyFail, IngestKey,
	// MaxBodyBytes, RetentionDays, RetentionMaxCount, EventTypePointer, and
	// Disabled. ID, Slug, UID, LastSeq, and CreatedAt are never changed, and an
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
	// GetInboxBySlug returns the inbox with that slug in the context's tenant (the
	// default tenant if it is not scoped), or ErrNotFound.
	GetInboxBySlug(ctx context.Context, slug string) (*Inbox, error)
	// ListInboxes pages through the inboxes in ID order (see ListParams).
	ListInboxes(ctx context.Context, p ListParams) ([]*Inbox, error)

	// CreateToken validates t, assigns ID and CreatedAt, and stores it, updating
	// *t. Every inbox in t.InboxIDs must exist (ErrNotFound otherwise, and
	// nothing is stored, and in a context scoped to a tenant that includes an
	// inbox of another tenant), and the hash must not belong to another token
	// (ErrConflict).
	CreateToken(ctx context.Context, t *Token) error
	// GetTokenByHash finds the token with this hash, or returns ErrNotFound. It
	// must be an indexed lookup, not a scan: the server calls it on every
	// authenticated request. It ignores the context's tenant (see Tenants).
	// A token whose ExpiresAt has passed, by the Store's clock, is ErrNotFound,
	// the same as one that never existed, so that an Authenticator built on the
	// Store cannot tell the two apart or serve an expired token.
	GetTokenByHash(ctx context.Context, hash []byte) (*Token, error)
	// ListTokens pages through tokens (see TokenListParams). It ignores the
	// context's tenant (see Tenants): it lists every token.
	ListTokens(ctx context.Context, p TokenListParams) ([]*Token, error)
	// DeleteToken removes a token, which stops working at once. It returns
	// ErrNotFound if there is none. It ignores the context's tenant (see Tenants).
	DeleteToken(ctx context.Context, id TokenID) error

	// Prune applies every inbox's retention policy as of now. A message is
	// deleted if either rule selects it: it is strictly older than
	// RetentionDays*24h before now, or it is not among the newest
	// RetentionMaxCount messages by Seq. Because ReceivedAt never decreases
	// with Seq, what is deleted is always a prefix of an inbox's messages.
	// Prune works in bounded batches. It returns the number of messages
	// deleted, and if ctx is cancelled partway, the count so far together
	// with the error. Pruning is idempotent.
	//
	// Prune is safe to call at the same time, from several goroutines and from
	// several Store values on one backend (every server of a deployment may run
	// the retention sweep). The deletes are per inbox and idempotent, so a message
	// that another call has removed first is not an error and is not deleted
	// twice; two calls that select the same messages may each count them, so the
	// counts of concurrent calls do not add up. Prune racing an Append or a
	// DeleteInbox never leaves a gap in what an inbox holds. Prune racing an
	// UpdateInbox that changes RetentionDays or RetentionMaxCount applies the old
	// limit or the new one and deletes nothing that both would keep; the next
	// Prune applies the new one.
	Prune(ctx context.Context, now time.Time) (deleted int64, err error)

	// Ping verifies the Store is reachable and usable, for readiness checks. It
	// reports the health of the Store as a whole, as seen from this server: any
	// error makes /readyz answer 503 (for one wrapping ErrStorageFull, saying there
	// is no room left to store deliveries; for any other, that the store is not
	// reachable), which takes the server out of a load balancer's rotation. Ping
	// may return ErrStorageFull when the Store itself has no room, as the SQLite
	// store does when its volume is nearly full or a write has just failed for
	// lack of space. It must not return it, or any error, because one tenant or
	// inbox is over a quota: every server sharing the Store would then fail
	// readiness at once, for every tenant. A write refused for a quota is
	// reported by Append, for that write (ErrStorageFull is answered with 503 and
	// the sender retries), or refused earlier by Hooks.BeforeIngest
	// (ErrQuotaExceeded, 402).
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
	//
	// A snapshot holds every tenant's data, so it is the operator's, not a
	// tenant's. A Store that holds the inboxes of several tenants refuses a context
	// scoped to one with WithTenant, returning an error that wraps ErrForbidden
	// (or hands back a snapshot of that tenant's inboxes alone, if it can). It
	// never gives a scoped caller the other tenants' data. The server does not
	// call Backup for a tenant's principal. A Store that holds one tenant's data
	// only has nothing to refuse.
	Backup(ctx context.Context) (snapshot io.ReadCloser, size int64, err error)
}

// TokenUseRecorder is an optional capability of a Store: remembering when a
// token last authenticated a request, which a Store shows as Token.LastUsedAt.
// The default Authenticator (pkg/auth/token) finds it with a type assertion;
// a Store without it has no last-used time and nothing fails.
//
// It is called at most once per token per interval (five minutes by default),
// from outside the request, and its failure is logged and otherwise ignored,
// so an implementation may be slow or may refuse under load without harm.
type TokenUseRecorder interface {
	// TouchToken records that token id was used at the given time. LastUsedAt
	// only moves forward: a call with a time that is not after the one already
	// recorded changes nothing and is not an error. It returns ErrNotFound if
	// there is no such token (it was revoked meanwhile). It does not change
	// anything else about the token.
	TouchToken(ctx context.Context, id TokenID, at time.Time) error
}

// EpochRenewer is an optional capability of a Store: giving every inbox a new
// epoch (Inbox.Epoch) without any other change. A Store restored by a means that
// is not part of this interface, such as a copy of its database file or a
// point-in-time recovery of its backend, holds sequence numbers that the server
// which wrote them may since have handed out differently, so the restore must
// renew the epochs before the Store serves again: a client holding a cursor from
// the old epoch is then told to start again (409 epoch_changed) and does not
// quietly skip or repeat messages. A snapshot taken by Backupper has new epochs
// already. Callers find this with a type assertion; the server does not call it,
// and an operator's restore procedure or an embedder's tooling does.
type EpochRenewer interface {
	// RenewEpochs gives every inbox of the Store, in every tenant, a new epoch,
	// whatever tenant the context is scoped to. Every call renews every epoch, so
	// calling it twice is safe, and no epoch it hands out was one the inbox had
	// before. It changes nothing else an inbox, message or token holds: the
	// messages, their sequence numbers (the next Append continues the count), the
	// tokens and the inboxes' settings are as they were. A Store may count the
	// change as an update of each inbox (increase its Version, as the SQLite
	// store does), so that an admin holding an older copy has to read it again.
	RenewEpochs(ctx context.Context) error
}

// UnverifiedCounter is an optional capability of a Store: how many unverified
// messages an inbox holds. The server uses it to cap them (see
// Options.MaxUnverified): with on_verify_fail=store, or a verifier of none, anyone
// holding the ingest key can store messages, and without a cap they fill the inbox
// and, under count-based retention, push out the genuine ones. A Store that does not
// implement it is not capped, and the server says so when it starts.
type UnverifiedCounter interface {
	// CountUnverified returns how many messages with Verified false the inbox
	// holds, or limit if there are more: the caller only asks whether a cap is
	// reached, and the answer must cost no more than limit rows however many
	// there are. It returns ErrNotFound if the inbox does not exist.
	CountUnverified(ctx context.Context, inboxID ID, limit int64) (int64, error)
}

// ReaderTracker is an optional capability of a Store: remembering how far each
// named reader of an inbox has read, so that a reader's lag can be shown. The
// server finds it with a type assertion; without it the reader operations answer 400
// "unsupported", so that nobody takes a reader to be tracked when it is not.
//
// A position is set only by an explicit call (AckReader), never by reading, so a
// read stays a read. It belongs to one epoch of its inbox: the sequence numbers of
// another epoch mean something else. A Store therefore drops every position when an
// inbox's epoch changes (RenewEpochs, and the epochs a Backupper snapshot is given),
// and never returns or counts a position whose epoch is not the inbox's current
// one. Deleting an inbox deletes its positions. Positions follow the tenant rules
// of the Store: an inbox outside the tenant of the context is ErrNotFound.
type ReaderTracker interface {
	// AckReader sets the position of the named reader of an inbox, creating the
	// reader if it is new, and returns it. epoch must be the inbox's current epoch or
	// the error wraps ErrEpochChanged and nothing is stored. seq must be from 0 to
	// the inbox's newest sequence number (a reader cannot have read what does not
	// exist yet) or the error wraps ErrInvalid; it may be lower than the last
	// position, which is how a reader starts over. A name that fails
	// ValidateReaderName is ErrInvalid. If the reader is new and the inbox already
	// has maxReaders of them, the error wraps ErrConflict and nothing is stored;
	// setting an existing reader never fails for the cap. maxReaders is checked and the
	// position written as one step, so concurrent calls cannot exceed it; zero or
	// less means no cap. An inbox that does not exist is ErrNotFound.
	AckReader(ctx context.Context, inboxID ID, name, epoch string, seq int64, maxReaders int) (ReaderPosition, error)
	// Readers returns the named readers of an inbox that have a position in its
	// current epoch, ordered by name. An inbox that does not exist is ErrNotFound.
	Readers(ctx context.Context, inboxID ID) ([]ReaderPosition, error)
	// ForgetReader deletes the named reader of an inbox. A reader that has no
	// position, and an inbox that does not exist, are ErrNotFound.
	ForgetReader(ctx context.Context, inboxID ID, name string) error
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
	//
	// Publish's rule makes the subscription exist from the moment Subscribe
	// returns, so Subscribe returns only once a Publish that begins afterwards,
	// on any node, will reach it. An in-process notifier registers it before
	// returning. A distributed one waits until its backend has confirmed the
	// subscription (for a pub/sub channel, the reply to the subscribe command),
	// but only briefly: Subscribe must not block the caller for long (the suite
	// fails one that takes 2 seconds). If it gives up waiting, or reuses a
	// connection whose subscription is not yet confirmed, a signal published in
	// the gap may be lost; that is the at-most-once delivery a distributed
	// notifier is allowed, and the reader finds the message on its next re-read,
	// which is why such a notifier must be deployed with a positive
	// Options.SSERecheckInterval.
	Subscribe(ctx context.Context, inboxID ID) (signal <-chan struct{}, cancel func())
}

// Authenticator resolves a request's credentials to a Principal.
//
// Authenticate runs before the server knows whether the caller is allowed to
// see anything, on requests from anyone, so an implementation that consults a
// database should be paired with rate limiting in front of it. That holds for
// every outcome, including the 404 an Unrouted error asks for.
type Authenticator interface {
	// Authenticate returns the caller's Principal, or an error wrapping
	// ErrUnauthenticated if the credentials are missing or invalid. Every such
	// failure must look the same to the caller: no error text, status, or
	// timing that says whether a token once existed, expired, or belongs to a
	// different inbox.
	//
	// The one exception is a request for a host that serves nobody: the error
	// may also implement Unrouted, and the server then answers 404 instead of
	// 401. Every request for that host, whatever its credential, gets that
	// answer, and no request for a host that is served does.
	//
	// Any other error is taken to mean the credential could not be checked (the
	// Store is down) and becomes a server error, not a 401. An implementation
	// must therefore wrap ErrUnauthenticated for an unknown token and must not
	// return some other error for it.
	Authenticate(r *http.Request) (Principal, error)
}

// Challenger is an optional capability of the error an Authenticator returns for
// a request it refuses. The server looks for it in the error chain (errors.As) of
// an error that wraps ErrUnauthenticated, and uses WWWAuthenticate as the value of
// the 401 response's WWW-Authenticate header in place of its own
// (`Bearer realm="hookinlet"`). It lets an Authenticator tell clients how to
// authenticate: for one that sits behind an OAuth server, a challenge such as
// `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`
// points them to the server's discovery document.
//
// The value must be one line of printable ASCII (no control characters, tab
// included), at most 4096 bytes, and start with an HTTP authentication scheme (an
// RFC 9110 token such as Bearer), followed by nothing or by a space and the
// scheme's parameters. The server does not parse the parameters. A value that
// does not meet that, or an empty one, is ignored and the server's own challenge
// is sent, so a mistake here cannot produce a malformed header. The same text is
// sent to every caller who is refused, so it must not depend on the request's
// credentials.
type Challenger interface {
	WWWAuthenticate() string
}

// Unrouted is an optional capability of the error an Authenticator returns for a
// request it refuses. The server looks for it in the error chain (errors.As) of an
// error that wraps ErrUnauthenticated, and if UnroutedHost reports true it answers
// 404 in place of 401, with the same body as its other 404s and no
// WWW-Authenticate header. It is for an embedder that serves many parties from
// one process and tells them apart by the request's host: a request for a host
// that serves nobody is answered as any other unknown address is, whatever
// credential it carries.
//
// The answer is for the host, not the credential. Every request for such a host
// must get it, whether it carries a valid token, an invalid one, or none, so that
// the 404 says nothing about any token. A refusal on a host that does serve
// someone must not report true: it stays a 401 that is the same for every bad
// credential. Reporting true does reveal that the host serves nobody (see
// docs/extending.md), and it does not replace rate limiting in front of
// Authenticate.
//
// Only an error that wraps ErrUnauthenticated is looked at. Returning ErrNotFound
// from Authenticate does not produce a 404: it is not ErrUnauthenticated, so it is
// still a server error.
type Unrouted interface {
	UnroutedHost() bool
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
// finite list of inboxes (an agent token scoped to some of them; not to be confused
// with a Store context scoped to a tenant). Anything that
// needs to list the inboxes a caller can read uses it to go straight to those,
// instead of walking every inbox and asking CanReadInbox, which would cost time in
// proportion to the number of inboxes and could not tell a caller that its list
// was complete without telling it how many others exist.
//
// InboxIDs must be exactly the IDs of the inboxes CanReadInbox accepts. A
// Principal that can read an unbounded set, or that can use the admin API, must not
// implement it. A set that is finite but large, such as every inbox of a group of
// callers, is not unbounded: a Principal may implement it with that set, resolved
// when the request is authenticated, provided it is exactly the set CanReadInbox
// accepts. Keep it small enough to hold in memory for each request.
type ScopedPrincipal interface {
	Principal
	// InboxIDs lists the inboxes the principal is limited to. It is asked once per
	// request that needs the list, with that request's context, so an implementation
	// that has to look the answer up may. An error is a server error for that request.
	InboxIDs(ctx context.Context) ([]ID, error)
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
	// Host is the host the delivery was addressed to: the request's Host header as
	// the server received it, in lower case, without a trailing dot, and with the
	// port only if the header had one ("hooks.example.com", "hooks.example.com:8443",
	// "[::1]:8080"). It is empty if the request sent none. It is never taken from
	// X-Forwarded-Host or any other header a proxy adds: behind a proxy that rewrites
	// Host, Host is what the proxy sent, and arranging for it to name the original
	// host is the embedder's concern. It came from the sender like every other field,
	// so it names a hostname the request used, not one the sender is entitled to.
	Host string
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
	// or ErrForbidden (403), or ErrNotFound, which is answered exactly as an
	// unknown ingest key is (404), so that an inbox that exists but is not served
	// to this caller looks as if it did not exist. Any other error is treated as
	// an internal failure.
	// A refusal may say when to retry (RefuseAfter, RetryAfterer). The request is
	// what is known of the delivery at that point; it grows by adding fields, which
	// is why it is a struct. AccessFrom(ctx) says which operation it is for.
	BeforeIngest(ctx context.Context, req *IngestRequest) error

	// AfterIngest runs after a delivery is committed or recognized as a
	// duplicate. created is false for a duplicate, so a metering hook can
	// avoid double counting. It must not block. The ctx may be cancelled as
	// soon as the HTTP response is sent; work that outlives the call must use
	// context.WithoutCancel.
	AfterIngest(ctx context.Context, in *Inbox, m *Message, created bool)
}

// AccessKind says what kind of access to an inbox Admitter.Admit is asked about.
type AccessKind string

const (
	// AccessRead is a read that answers at once and holds a read slot while it
	// does: listing or fetching messages, inbox info, and the MCP query, get and
	// list tools (and a wait with a timeout of zero). For the MCP list tool the
	// inbox is nil, because the call is not about one inbox.
	AccessRead AccessKind = "read"
	// AccessStream is a stream (server-sent events) that stays open until the
	// client leaves. It is released when the stream ends.
	AccessStream AccessKind = "stream"
	// AccessWait is an MCP wait, which holds a connection open for up to a minute
	// to wait for messages. It is released when the wait ends.
	AccessWait AccessKind = "wait"
)

// Admitter is an optional capability of Hooks, found by type assertion: a Hooks
// that also implements it is asked before every read, stream and wait. Hooks
// itself does not change, and a Hooks without it admits everything, which is the
// open-source default.
//
// It is how an embedder limits what a caller may use at once (streams, waits,
// reads in flight) by something the server does not know, such as who the caller
// is part of. The server's own limits (MaxConcurrentReads, MaxStreams,
// MaxStreamsPerInbox) apply as well, whatever Admit says.
type Admitter interface {
	// Admit runs after the caller has been authenticated and allowed to read the
	// inbox, and before the access takes a slot of the server's own. It may refuse
	// by returning an error wrapping ErrRateLimited (429), ErrQuotaExceeded (402)
	// or ErrForbidden (403); any other error is an internal failure (500). It
	// returns a release function, which may be nil, called once when the access
	// ends (when the answer has been written, or the stream or wait has finished),
	// including when the access fails later. in is nil only where AccessKind says
	// so. Admit must not block for long: a read waits on it before any slot is held.
	// A refusal may say when to retry (RefuseAfter, RetryAfterer), and AccessFrom(ctx)
	// says which operation or MCP tool is asking and the request's id.
	Admit(ctx context.Context, p Principal, in *Inbox, kind AccessKind) (release func(), err error)
}

// UnverifiedLimiter is an optional capability of Hooks, found by type assertion:
// a Hooks that also implements it says, per inbox, how many unverified messages
// the inbox may keep, in place of the server's own limit (Options.MaxUnverified).
// Hooks itself does not change. It is how an embedder gives inboxes different
// caps by something the server does not know, such as the plan they belong to.
// It has an effect only if the Store implements UnverifiedCounter.
type UnverifiedLimiter interface {
	// MaxUnverified returns the most unverified messages in may keep: a positive
	// number is the cap, 0 means the server's limit applies, and a negative number
	// means this inbox has no cap. It is asked only for a delivery that is
	// unverified, before the server takes the lock that serializes such appends,
	// so it may look the answer up, but must not block for long.
	MaxUnverified(ctx context.Context, in *Inbox) int
}

// IngestOutcome says how a delivery ended, for IngestObserver. New outcomes may be
// added; an observer should treat one it does not know as a refusal it does not
// count separately.
type IngestOutcome string

const (
	// IngestStored: the message was committed (200).
	IngestStored IngestOutcome = "stored"
	// IngestDuplicate: a repeat of a verified delivery the inbox already holds; it
	// was acknowledged (200) and nothing new was stored.
	IngestDuplicate IngestOutcome = "duplicate"
	// IngestHandshake: a verified handshake (Slack's url_verification) was answered
	// (200) and not stored.
	IngestHandshake IngestOutcome = "handshake"
	// IngestVerificationFailed: the signature did not verify and the inbox refuses
	// such deliveries (401). With on_verify_fail=store the delivery is stored
	// unverified instead, and the outcome is IngestStored with Verified false.
	IngestVerificationFailed IngestOutcome = "verification_failed"
	// IngestRateLimited, IngestQuotaExceeded and IngestForbidden: Hooks.BeforeIngest
	// refused the delivery with an error wrapping ErrRateLimited (429),
	// ErrQuotaExceeded (402) or ErrForbidden (403).
	IngestRateLimited   IngestOutcome = "rate_limited"
	IngestQuotaExceeded IngestOutcome = "quota_exceeded"
	IngestForbidden     IngestOutcome = "forbidden"
	// IngestUnverifiedLimit: the delivery would have been stored unverified and the
	// inbox holds as many unverified messages as it may (429).
	IngestUnverifiedLimit IngestOutcome = "unverified_limit"
	// IngestStorageFull: the Store had no room for it (503).
	IngestStorageFull IngestOutcome = "storage_full"
	// IngestOverloaded: the server was holding as many bodies in memory as it may
	// and told the sender to come back (503).
	IngestOverloaded IngestOutcome = "overloaded"
	// IngestTooLarge: the body was over the inbox's MaxBodyBytes (413).
	IngestTooLarge IngestOutcome = "too_large"
	// IngestBadRequest: the body could not be read, for instance because the sender
	// went away part way (400).
	IngestBadRequest IngestOutcome = "bad_request"
	// IngestNotFound: the inbox is disabled, or was deleted while the delivery was
	// being received (404, as for a key that names no inbox).
	IngestNotFound IngestOutcome = "not_found"
	// IngestError: something failed on the server's side, including a BeforeIngest
	// error that wraps none of the refusals above (500). The cause is in the log.
	IngestError IngestOutcome = "error"
)

// VerifyFailure says, in a few fixed words, how a signature failed to verify, for
// IngestEvent. It names no header and no sender. The values are fixed; a verifier
// that does not classify its failures leaves it empty.
type VerifyFailure string

const (
	// VerifyFailureMissing: a header the verifier needs was not sent.
	VerifyFailureMissing VerifyFailure = "missing"
	// VerifyFailureMalformed: a header was sent but could not be read as what the
	// verifier expects (wrong prefix or encoding, a bad timestamp, or the header
	// repeated).
	VerifyFailureMalformed VerifyFailure = "malformed"
	// VerifyFailureMismatch: the signature or token was well formed and did not
	// match.
	VerifyFailureMismatch VerifyFailure = "mismatch"
	// VerifyFailureStale: the signature matched but its timestamp was outside the
	// tolerance window. A forged delivery never has this kind: the signature is
	// checked first.
	VerifyFailureStale VerifyFailure = "stale"
)

// IngestEvent is how one delivery to a known inbox ended, for IngestObserver. It
// grows by adding fields, which is why it is a struct. An observer must not modify
// it or the Inbox it points to.
//
// It carries nothing the sender sent beyond its size, address and host: no headers, no
// body, and not the reason text of a signature failure (the server logs that
// itself), only its VerifyFailure kind, so that what an embedder logs or forwards
// from it cannot leak them.
type IngestEvent struct {
	// Inbox is the inbox the delivery was for. It is never nil; its Slug, Tenant and
	// UID say which inbox it is.
	Inbox *Inbox
	// Outcome is how the delivery ended, and Status the HTTP status the sender was
	// sent.
	Outcome IngestOutcome
	Status  int
	// Size is how many body bytes were read: 0 for a delivery refused before its
	// body was read (one whose Content-Length is over the limit, or a disabled
	// inbox), and as far as reading got for one refused while it was read.
	Size int64
	// SourceIP is where the delivery came from, as in IngestRequest. It may be
	// empty.
	SourceIP string
	// Host is the host the delivery was addressed to, as in IngestRequest. It was
	// chosen by the sender, so treat it as input (as a metric label it can grow
	// without bound). It may be empty.
	Host string
	// Seq is the message's sequence number for IngestStored, and for
	// IngestDuplicate that of the message it repeats; 0 otherwise.
	Seq int64
	// Verified is whether the stored message is verified, for IngestStored and
	// IngestDuplicate (a duplicate is always verified); false otherwise.
	Verified bool
	// VerifyFailure says how verification failed, set only when Outcome is
	// IngestVerificationFailed. It is empty when the verifier does not say (a
	// custom verifier may not).
	VerifyFailure VerifyFailure
}

// IngestObserver is an optional capability of Hooks, found by type assertion: a
// Hooks that also implements it is told how every delivery to a known inbox ended,
// including those refused or failed, which AfterIngest never sees. Hooks itself
// does not change, and a Hooks without it changes nothing. It is how an embedder
// counts, per inbox or tenant, what the server's metrics count only in total, for
// instance to alert when verification failures for one inbox spike.
//
// A delivery whose ingest key names no inbox is not observed, nor is one refused
// before the inbox is looked up (a wrong method): there is no inbox to attribute it
// to, and an observer that saw them would see whatever anyone guesses.
type IngestObserver interface {
	// ObserveIngest is called once for every delivery to a known inbox, after the
	// outcome is decided, after AfterIngest for a stored message or duplicate, and
	// before the response is written. It runs on the request's goroutine and must not
	// block: a slow observer delays the response. The ctx may be cancelled already
	// (the sender may have gone away) or soon after; work that outlives the call
	// must use context.WithoutCancel. A panic in it is logged and swallowed, and does
	// not change the response.
	ObserveIngest(ctx context.Context, ev *IngestEvent)
}

// SecretBox seals secrets for storage at rest.
type SecretBox interface {
	// Seal encrypts plaintext and binds it to context: the same context must be
	// given to Open. The caller chooses what the value belongs to (for a verifier
	// secret, VerifierSecretContext(uid), the inbox's UID), so that a sealed value
	// copied onto another row does not open there. Sealing the same plaintext twice yields different ciphertexts.
	Seal(plaintext, context []byte) ([]byte, error)
	// Open decrypts ciphertext produced by Seal, given the context it was sealed
	// with. It returns an error if the ciphertext was tampered with, truncated,
	// sealed under another key, or sealed for another context.
	Open(ciphertext, context []byte) ([]byte, error)
}

// ContextSecretBox is an optional capability of a SecretBox, found by type
// assertion: a SecretBox that also implements it is given the context of the work
// a secret is sealed or opened for, and the server calls these methods in place of
// Seal and Open. A SecretBox without it is called as before. It is for a box that
// does I/O, such as one that asks a key service to unwrap a data key: it can stop
// when the context is cancelled (the sender of a delivery went away) or past its
// deadline, and read values the embedder put on it.
//
// The context is that of the work: for a delivery, the request's; for an admin API
// call, the request's; for the readiness check, the check's own, with a deadline;
// for Server.CheckSecrets and the command line, the caller's. Whatever the work, the
// box is given it scoped (WithTenant) to the tenant of the inbox whose secret it is,
// the default tenant as "", so TenantFrom always reports scoped and a box can tell
// the default tenant from a context that says nothing. Only the box's context is
// scoped so: the Store calls of the same work keep theirs (a delivery's are not
// scoped at all).
//
// binding is what Seal and Open call context: the bytes the value is bound to,
// such as VerifierSecretContext(uid). Results must be the same as Seal's and
// Open's: a value sealed by one method opens with either. An implementation that
// does I/O returns an error that wraps ctx.Err() when it gives up because of the
// context. Call these through SealWith and OpenWith, not directly.
type ContextSecretBox interface {
	SecretBox
	// SealContext is Seal, given the context.
	SealContext(ctx context.Context, plaintext, binding []byte) ([]byte, error)
	// OpenContext is Open, given the context.
	OpenContext(ctx context.Context, ciphertext, binding []byte) ([]byte, error)
}

// SealWith seals plaintext with box, bound to binding (see SecretBox.Seal), giving
// box the context if it is a ContextSecretBox. It is how the server seals every
// secret.
func SealWith(ctx context.Context, box SecretBox, plaintext, binding []byte) ([]byte, error) {
	if cb, ok := box.(ContextSecretBox); ok {
		return cb.SealContext(ctx, plaintext, binding)
	}
	return box.Seal(plaintext, binding)
}

// OpenWith opens ciphertext with box, given the binding it was sealed with (see
// SecretBox.Open), giving box the context if it is a ContextSecretBox. It is how
// the server opens every secret.
func OpenWith(ctx context.Context, box SecretBox, ciphertext, binding []byte) ([]byte, error) {
	if cb, ok := box.(ContextSecretBox); ok {
		return cb.OpenContext(ctx, ciphertext, binding)
	}
	return box.Open(ciphertext, binding)
}
