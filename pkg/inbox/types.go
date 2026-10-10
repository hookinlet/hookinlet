// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package inbox defines hookinlet's domain types and the interfaces that
// storage, notification, authentication, and secret handling implement. It is
// the public contract: the open-source defaults and any third-party
// implementation (for example a hosted multi-node one) satisfy the same
// interfaces and pass the same conformance suites in pkg/inboxtest.
//
// Nothing in this package knows about organizations, users, plans, or billing.
package inbox

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"
)

// ID identifies an inbox. It is opaque to callers and never appears in a
// public API; the slug is the external name.
type ID int64

// Limits and defaults.
const (
	// DefaultMaxBodyBytes is the per-inbox body limit when none is set.
	DefaultMaxBodyBytes int64 = 1 << 20
	// MaxBodyBytesCeiling is the largest per-inbox body limit allowed.
	MaxBodyBytesCeiling int64 = 10 << 20

	// DefaultQueryLimit and MaxQueryLimit bound Query.Limit.
	DefaultQueryLimit = 50
	MaxQueryLimit     = 500

	// DefaultListLimit and MaxListLimit bound ListParams.Limit.
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// What to do with a delivery whose signature does not verify.
const (
	OnVerifyFailReject = "reject" // respond 401, store nothing
	OnVerifyFailStore  = "store"  // store it with Verified=false
)

var verifierNames = map[string]struct{}{
	"none": {}, "token": {}, "hmac": {}, "stripe": {}, "github": {}, "slack": {}, "standardwebhooks": {},
}

// KnownVerifier reports whether name is a built-in verifier preset. Embedders
// that register extra verifiers manage their own names; this check applies to
// the open-source Store's validation.
func KnownVerifier(name string) bool {
	_, ok := verifierNames[name]
	return ok
}

var slugRE = regexp.MustCompile(`^[a-z0-9-]{3,64}$`)

// ValidSlug reports whether s is a valid inbox slug: 3 to 64 characters from
// a-z, 0-9, and hyphen.
func ValidSlug(s string) bool { return slugRE.MatchString(s) }

// Inbox is a named destination for webhooks.
type Inbox struct {
	ID   ID
	Slug string // immutable after creation; the external name
	Name string

	// Verifier selects the signature scheme: none, token, hmac, stripe,
	// github, slack, or standardwebhooks.
	Verifier string
	// VerifierConfig holds non-secret verifier parameters as a JSON object.
	VerifierConfig json.RawMessage
	// VerifierSecret is the verifier secret sealed by a SecretBox. A Store
	// persists it verbatim and never sees the plaintext.
	VerifierSecret []byte
	// OnVerifyFail is OnVerifyFailReject or OnVerifyFailStore.
	OnVerifyFail string

	// AgentTokenHash is the hash of the inbox's agent token. Only the hash is
	// stored; the token is shown once at creation or rotation.
	AgentTokenHash []byte

	// MaxBodyBytes limits accepted body size, up to MaxBodyBytesCeiling.
	MaxBodyBytes int64
	// RetentionDays prunes messages older than this many days. 0 means keep
	// forever by age.
	RetentionDays int
	// RetentionMaxCount keeps at most this many of the newest messages. 0 means
	// no count limit.
	RetentionMaxCount int64
	// EventTypePointer is a JSON pointer (for example "/type") used to extract
	// the event type from JSON bodies on generic inboxes. Empty means none.
	EventTypePointer string

	// LastSeq is the sequence number of the newest message ever appended.
	// Only Append changes it; CreateInbox and UpdateInbox ignore the value
	// supplied by the caller.
	LastSeq int64

	Disabled  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ApplyDefaults fills zero-valued optional fields. Every Store calls it before
// validating on create and update so defaults are identical across backends.
func (in *Inbox) ApplyDefaults() {
	if in.OnVerifyFail == "" {
		in.OnVerifyFail = OnVerifyFailReject
	}
	if in.MaxBodyBytes == 0 {
		in.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if len(in.VerifierConfig) == 0 {
		in.VerifierConfig = json.RawMessage(`{}`)
	}
}

// Validate checks the inbox's fields. Errors wrap ErrInvalid. Call
// ApplyDefaults first.
func (in *Inbox) Validate() error {
	switch {
	case !ValidSlug(in.Slug):
		return Invalidf("slug must be 3-64 characters from a-z, 0-9, and hyphen")
	case in.Name == "" || len(in.Name) > 200:
		return Invalidf("name must be 1-200 characters")
	case !KnownVerifier(in.Verifier):
		return Invalidf("unknown verifier %q", in.Verifier)
	case in.OnVerifyFail != OnVerifyFailReject && in.OnVerifyFail != OnVerifyFailStore:
		return Invalidf("on_verify_fail must be %q or %q", OnVerifyFailReject, OnVerifyFailStore)
	case len(in.AgentTokenHash) == 0:
		return Invalidf("agent token hash is required")
	case in.MaxBodyBytes < 1 || in.MaxBodyBytes > MaxBodyBytesCeiling:
		return Invalidf("max_body_bytes must be between 1 and %d", MaxBodyBytesCeiling)
	case in.RetentionDays < 0:
		return Invalidf("retention_days must not be negative")
	case in.RetentionMaxCount < 0:
		return Invalidf("retention_max_count must not be negative")
	case in.EventTypePointer != "" && in.EventTypePointer[0] != '/':
		return Invalidf("event_type_pointer must be a JSON pointer starting with /")
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(in.VerifierConfig, &cfg); err != nil || cfg == nil {
		return Invalidf("verifier_config must be a JSON object")
	}
	return nil
}

// NewMessage is a delivery to be appended to an inbox.
type NewMessage struct {
	// ReceivedAt is when the delivery arrived. Zero means the Store's clock.
	ReceivedAt time.Time
	Method     string
	// PathSuffix is the part of the ingest path after the inbox key, without a
	// leading slash.
	PathSuffix string
	// Query is the raw query string without the leading "?".
	Query string
	// Headers are the request headers. A Store canonicalizes the keys and drops
	// AlwaysRedactedHeaders; see RedactHeaders.
	Headers     http.Header
	ContentType string
	// Body is the exact bytes received. It is stored byte for byte, including
	// invalid UTF-8, NUL bytes, and the empty body.
	Body        []byte
	Verified    bool
	VerifyError string
	// DeliveryID, when non-empty, deduplicates deliveries within an inbox.
	DeliveryID string
	EventType  string
	SourceIP   string
}

// Validate checks the fields a Store cannot sensibly default.
func (m *NewMessage) Validate() error {
	if m.Method == "" {
		return Invalidf("method is required")
	}
	return nil
}

// Message is a stored delivery.
type Message struct {
	InboxID ID
	// Seq is the per-inbox sequence number, starting at 1. It is the cursor.
	Seq        int64
	ReceivedAt time.Time
	Method     string
	PathSuffix string
	Query      string
	Headers    http.Header
	// ContentType is the Content-Type of the delivery.
	ContentType string
	// Body is nil, not empty, when the Query asked to omit bodies.
	Body        []byte
	Size        int64 // size of the stored body, set even when Body is omitted
	Verified    bool
	VerifyError string
	DeliveryID  string
	EventType   string
	SourceIP    string
}

// Query selects messages from one inbox. Filters combine with AND, except
// Types, whose values combine with OR.
//
// Ordering and paging: with After set, results are ascending by Seq starting
// just after it (the polling pattern). With Before set, results are
// descending from just before it. With neither, results are the newest
// messages, descending. Setting both is invalid.
type Query struct {
	After  *int64 // exclusive; 0 means from the beginning
	Before *int64 // exclusive
	// Limit caps the result size. 0 means DefaultQueryLimit; values above
	// MaxQueryLimit are clamped. Negative is invalid.
	Limit int
	// Types matches any of these event types.
	Types []string
	// Since is inclusive and Until exclusive, both compared with ReceivedAt.
	// The zero time means unbounded.
	Since, Until time.Time
	Verified     *bool
	DeliveryID   string
	// OmitBody leaves Message.Body nil for lighter results.
	OmitBody bool
}

// Validate checks the query. Errors wrap ErrInvalid.
func (q Query) Validate() error {
	switch {
	case q.After != nil && q.Before != nil:
		return Invalidf("after and before are mutually exclusive")
	case q.After != nil && *q.After < 0, q.Before != nil && *q.Before < 0:
		return Invalidf("cursors must not be negative")
	case q.Limit < 0:
		return Invalidf("limit must not be negative")
	}
	return nil
}

// EffectiveLimit returns Limit with the default and maximum applied.
func (q Query) EffectiveLimit() int {
	switch {
	case q.Limit == 0:
		return DefaultQueryLimit
	case q.Limit > MaxQueryLimit:
		return MaxQueryLimit
	}
	return q.Limit
}

// Ascending reports whether results are ordered by increasing Seq.
func (q Query) Ascending() bool { return q.After != nil }

// Page is the result of a Query.
type Page struct {
	Messages []*Message
	// LatestSeq is the inbox's newest sequence number as of the same snapshot
	// as Messages. It is 0 for an inbox that has never received a message.
	LatestSeq int64
}

// InboxStats summarizes an inbox's stored messages.
type InboxStats struct {
	// MessageCount is the exact number of messages currently stored.
	MessageCount int64
	// OldestSeq is the lowest stored sequence number, or 0 if none.
	OldestSeq int64
	// LatestSeq equals Inbox.LastSeq.
	LatestSeq int64
}

// ListParams pages through inboxes in ascending ID order.
type ListParams struct {
	// AfterID returns inboxes with an ID greater than this. 0 starts at the
	// beginning.
	AfterID ID
	// Limit defaults to DefaultListLimit and is clamped to MaxListLimit.
	Limit int
}

// EffectiveLimit returns Limit with the default and maximum applied.
func (p ListParams) EffectiveLimit() int {
	switch {
	case p.Limit <= 0:
		return DefaultListLimit
	case p.Limit > MaxListLimit:
		return MaxListLimit
	}
	return p.Limit
}
