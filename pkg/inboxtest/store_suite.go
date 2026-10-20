// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// baseTime anchors every timestamp the suite generates. All times use
// microsecond precision or coarser, the finest a Store is required to keep.
var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// StoreSuite runs the inbox.Store conformance tests.
//
// newStore must return a new, empty Store on every call, driven by the given
// clock: the Store must use now for every timestamp it assigns. The suite
// closes each Store itself, so newStore must not register its own Close.
//
// Checks that need something only the Store's own test can supply take a
// StoreOptions: a Store that is full (MakeFull), a second Store value on the same
// backend (Peer), and a Store that must be told of a tenant before it holds
// inboxes for it (PrepareTenant). Pass at most one; with none, those cases run
// without the extra help (or are skipped). StoreSuiteWith is the same call with the
// options not optional.
func StoreSuite(t *testing.T, newStore func(t *testing.T, now func() time.Time) inbox.Store, opts ...StoreOptions) {
	t.Helper()
	if len(opts) > 1 {
		t.Fatalf("StoreSuite takes at most one StoreOptions, got %d", len(opts))
	}
	var o StoreOptions
	if len(opts) == 1 {
		o = opts[0]
	}
	runStoreSuite(t, newStore, o)
}

func runStoreSuite(t *testing.T, newStore func(t *testing.T, now func() time.Time) inbox.Store, opts StoreOptions) {
	t.Helper()
	// peers makes the other Store values on the backend s uses, if the options
	// say how. The suite closes them.
	peers := func(t *testing.T, s inbox.Store) []inbox.Store {
		t.Helper()
		if opts.Peer == nil {
			return nil
		}
		p := opts.Peer(t, s)
		if p == nil {
			t.Fatal("StoreOptions.Peer returned a nil Store")
		}
		t.Cleanup(func() { _ = p.Close() })
		return []inbox.Store{p}
	}
	cases := []struct {
		name string
		fn   func(*testing.T, inbox.Store, *Clock)
	}{
		{"CreateInbox", testCreateInbox},
		{"CreateInboxValidation", testCreateInboxValidation},
		{"IngestKeys", testIngestKeys},
		{"InboxReturnsDoNotAlias", testInboxAliasing},
		{"InboxSecretsAreRedacted", testInboxSecretsRedacted},
		{"UpdateInbox", testUpdateInbox},
		{"UpdateInboxVersioning", testUpdateVersioning},
		{"Epochs", testEpochs},
		{"UIDs", testUIDs},
		{"Tenants", func(t *testing.T, s inbox.Store, clk *Clock) { testTenants(t, s, clk, newTenantPrep(opts)) }},
		{"TokensAreNotScopedByTenant", func(t *testing.T, s inbox.Store, clk *Clock) {
			testTokensAreNotScopedByTenant(t, s, clk, newTenantPrep(opts))
		}},
		{"OldestSeqInAPage", testOldestSeqInAPage},
		{"Tokens", testTokens},
		{"TokensOfADeletedInbox", testTokensOfADeletedInbox},
		{"TokenExpiry", testTokenExpiry},
		{"TokenLastUsed", testTokenLastUsed},
		{"DeleteInbox", testDeleteInbox},
		{"DeleteInboxRacingAppendsAndTokens", testDeleteRacesWrites},
		{"ListInboxes", testListInboxes},
		{"Ping", testPing},
		{"AppendBasics", testAppendBasics},
		{"ReceivedAtIsAssignedByTheStore", testReceivedAt},
		{"AppendDedupe", testAppendDedupe},
		{"AppendDedupeKey", testAppendDedupeKey},
		{"AppendDedupeIgnoresUnverified", testDedupeIgnoresUnverified},
		{"AppendDedupeConcurrent", testAppendDedupeConcurrent},
		{"BodyAndFieldRoundTrip", testRoundTrip},
		{"MessagesDoNotAlias", testMessageAliasing},
		{"HeaderRedaction", testHeaderRedaction},
		{"GetAndStats", testGetAndStats},
		{"MissingInbox", testMissingInbox},
		{"QueryFilters", testQueryFilters},
		{"QueryReturnsExactlyTheLimit", testQueryExactness},
		{"QueryLatestSeqIsNotNewerThanTheRows", testQuerySnapshot},
		{"QueryBodyLimit", testQueryBodyLimit},
		{"QueryLimits", testQueryLimits},
		{"QueryInvalid", testQueryInvalid},
		{"QueryPaging", testQueryPaging},
		{"RetentionByAge", testRetentionByAge},
		{"RetentionByCount", testRetentionByCount},
		{"RetentionCombined", testRetentionCombined},
		{"RetentionDeletesAPrefix", testRetentionIsPrefix},
		{"RetentionAndDedupe", testRetentionAndDedupe},
		{"PruneCancelledPartway", testPruneCancelledPartway},
		{"ConcurrentPrune", func(t *testing.T, s inbox.Store, clk *Clock) { testConcurrentPrune(t, s, clk, peers(t, s)) }},
		{"PruneRacingARetentionChange", func(t *testing.T, s inbox.Store, clk *Clock) { testPruneRacesARetentionChange(t, s, clk, peers(t, s)) }},
		{"AppendToAFullStoreIsErrStorageFull", func(t *testing.T, s inbox.Store, _ *Clock) {
			if opts.MakeFull == nil {
				t.Skip("StoreOptions.MakeFull is not set; see StoreSuiteWith")
			}
			checkStorageFull(t, s, func() func() { return opts.MakeFull(t, s) })
		}},
		{"ConcurrentAppendsAreGapless", testConcurrentGapless},
		{"CursorSafety", testCursorSafety},
		{"UpdateDoesNotDisturbAppends", testUpdateDuringAppends},
		{"CancelledContext", testCancelledContext},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := NewClock(baseTime)
			s := newStore(t, clk.Now)
			t.Cleanup(func() { _ = s.Close() })
			c.fn(t, s, clk)
		})
	}
}

// ---- helpers ----

// tokenHash derives a distinct, valid token hash from a label, since the Store
// requires hashes to be unique across inboxes.
func tokenHash(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

func mkInbox(slug string, mods ...func(*inbox.Inbox)) *inbox.Inbox {
	in := &inbox.Inbox{
		Slug:      slug,
		IngestKey: slug,
		Name:      "Inbox " + slug,
		Verifier:  "none",
	}
	for _, m := range mods {
		m(in)
	}
	return in
}

func createInbox(t *testing.T, s inbox.Store, slug string, mods ...func(*inbox.Inbox)) *inbox.Inbox {
	t.Helper()
	in := mkInbox(slug, mods...)
	if err := s.CreateInbox(t.Context(), in); err != nil {
		t.Fatalf("CreateInbox(%s): %v", slug, err)
	}
	return in
}

func nm(mods ...func(*inbox.NewMessage)) *inbox.NewMessage {
	m := &inbox.NewMessage{
		Method:      "POST",
		Headers:     http.Header{"Content-Type": {"text/plain"}},
		ContentType: "text/plain",
		Body:        []byte("body"),
		Verified:    true,
	}
	for _, mod := range mods {
		mod(m)
	}
	return m
}

func appendMsg(t *testing.T, s inbox.Store, id inbox.ID, mods ...func(*inbox.NewMessage)) *inbox.Message {
	t.Helper()
	msg, created, err := s.Append(t.Context(), id, nm(mods...))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !created {
		t.Fatalf("Append(seq %d) reported created=false", msg.Seq)
	}
	return msg
}

// appendAt sets the Store's clock to at, then appends.
func appendAt(t *testing.T, s inbox.Store, clk *Clock, id inbox.ID, at time.Time, mods ...func(*inbox.NewMessage)) *inbox.Message {
	t.Helper()
	clk.Set(at)
	return appendMsg(t, s, id, mods...)
}

// appendN appends n messages numbered 1..n to the inbox, letting mod
// customize message i.
func appendN(t *testing.T, s inbox.Store, id inbox.ID, n int, mod func(i int, m *inbox.NewMessage)) {
	t.Helper()
	for i := 1; i <= n; i++ {
		appendMsg(t, s, id, func(m *inbox.NewMessage) {
			m.Body = []byte(fmt.Sprintf("message-%d", i))
			if mod != nil {
				mod(i, m)
			}
		})
	}
}

func seqsOf(msgs []*inbox.Message) []int64 {
	out := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}

func seqRange(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func seqRangeDesc(from, to int64) []int64 {
	var out []int64
	for i := from; i >= to; i-- {
		out = append(out, i)
	}
	return out
}

func wantSeqs(t *testing.T, what string, got []*inbox.Message, want []int64) {
	t.Helper()
	g := seqsOf(got)
	if len(g) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("%s: seqs = %v, want %v", what, g, want)
	}
}

func query(t *testing.T, s inbox.Store, id inbox.ID, q inbox.Query) *inbox.Page {
	t.Helper()
	p, err := s.Query(t.Context(), id, q)
	if err != nil {
		t.Fatalf("Query(%+v): %v", q, err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// sameInbox compares every field. VerifierConfig is compared as JSON, since a
// backend may normalize whitespace and key order.
func sameInbox(t *testing.T, what string, got, want *inbox.Inbox) {
	t.Helper()
	ok := got.ID == want.ID && got.Slug == want.Slug && got.Name == want.Name && got.Version == want.Version &&
		got.Verifier == want.Verifier && jsonEqual(got.VerifierConfig, want.VerifierConfig) &&
		bytes.Equal(got.VerifierSecret, want.VerifierSecret) && got.OnVerifyFail == want.OnVerifyFail &&
		got.IngestKey == want.IngestKey && got.MaxBodyBytes == want.MaxBodyBytes &&
		got.RetentionDays == want.RetentionDays && got.RetentionMaxCount == want.RetentionMaxCount &&
		got.EventTypePointer == want.EventTypePointer && got.LastSeq == want.LastSeq &&
		got.Disabled == want.Disabled && got.CreatedAt.Equal(want.CreatedAt) && got.UpdatedAt.Equal(want.UpdatedAt)
	if !ok {
		t.Errorf("%s: inbox mismatch\n got: %#v\nwant: %#v", what, *got, *want)
	}
}

func isErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: error = %v, want %v", what, err, want)
	}
}

// ---- inbox management ----

func testCreateInbox(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	clk.Set(baseTime.Add(1500 * time.Nanosecond)) // sub-microsecond part must be truncated
	in := mkInbox("alpha", func(in *inbox.Inbox) {
		in.LastSeq = 99 // callers cannot set the counter
		in.Version = 42 // nor the version
		in.VerifierSecret = []byte("sealed-secret-bytes")
		in.VerifierConfig = json.RawMessage(`{"header": "X-Sig", "algorithm": "sha256"}`)
		in.RetentionDays = 7
		in.RetentionMaxCount = 1000
		in.EventTypePointer = "/type"
		in.OnVerifyFail = inbox.OnVerifyFailStore
	})
	if err := s.CreateInbox(ctx, in); err != nil {
		t.Fatal(err)
	}
	if in.ID == 0 || in.LastSeq != 0 || in.Version != 1 {
		t.Errorf("CreateInbox did not populate the inbox: id=%d lastSeq=%d version=%d", in.ID, in.LastSeq, in.Version)
	}
	wantTime := baseTime.Add(time.Microsecond)
	if !in.CreatedAt.Equal(wantTime.Truncate(time.Microsecond)) || !in.UpdatedAt.Equal(in.CreatedAt) ||
		in.CreatedAt.Location() != time.UTC {
		t.Errorf("timestamps = %v / %v, want %v in UTC (truncated to microseconds)", in.CreatedAt, in.UpdatedAt, wantTime)
	}

	got, err := s.GetInbox(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	sameInbox(t, "GetInbox", got, in)
	bySlug, err := s.GetInboxBySlug(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	sameInbox(t, "GetInboxBySlug", bySlug, in)
	resolved, err := s.ResolveIngestKey(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	sameInbox(t, "ResolveIngestKey", resolved, in)

	// Defaults apply to omitted fields.
	d := createInbox(t, s, "defaults")
	if d.OnVerifyFail != inbox.OnVerifyFailReject || d.MaxBodyBytes != inbox.DefaultMaxBodyBytes ||
		!jsonEqual(d.VerifierConfig, json.RawMessage(`{}`)) {
		t.Errorf("defaults not applied: %#v", *d)
	}

	// A taken slug conflicts, and distinct inboxes get distinct ids.
	isErr(t, "duplicate slug", s.CreateInbox(ctx, mkInbox("alpha", func(in *inbox.Inbox) { in.IngestKey = "different-key" })), inbox.ErrConflict)
	isErr(t, "duplicate ingest key", s.CreateInbox(ctx, mkInbox("bravo", func(in *inbox.Inbox) { in.IngestKey = "alpha" })), inbox.ErrConflict)
	if _, err := s.GetInboxBySlug(ctx, "bravo"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("a conflicting create left an inbox behind: %v", err)
	}
	if d.ID == in.ID {
		t.Error("two inboxes share an id")
	}

	for name, call := range map[string]func() error{
		"GetInbox":          func() error { _, err := s.GetInbox(ctx, in.ID+1000); return err },
		"GetInboxBySlug":    func() error { _, err := s.GetInboxBySlug(ctx, "nope"); return err },
		"ResolveIngestKey":  func() error { _, err := s.ResolveIngestKey(ctx, "nope"); return err },
		"ResolveEmptyKey":   func() error { _, err := s.ResolveIngestKey(ctx, ""); return err },
		"ResolveOtherCase":  func() error { _, err := s.ResolveIngestKey(ctx, "ALPHA"); return err },
		"ResolveWithSuffix": func() error { _, err := s.ResolveIngestKey(ctx, "alpha/extra"); return err },
	} {
		isErr(t, name, call(), inbox.ErrNotFound)
	}
}

// An inbox is reached at its ingest key, a value of its own that the caller must
// supply: a Store does not make one up, because a key that is not random is a
// name anyone can guess.
func testIngestKeys(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	isErr(t, "no ingest key", s.CreateInbox(ctx, mkInbox("keys-none", func(in *inbox.Inbox) { in.IngestKey = "" })), inbox.ErrInvalid)

	plain := createInbox(t, s, "keys-plain")
	custom := createInbox(t, s, "keys-custom", func(in *inbox.Inbox) { in.IngestKey = "Zq_9-Mixed" })
	got, err := s.ResolveIngestKey(ctx, "Zq_9-Mixed")
	if err != nil || got.ID != custom.ID {
		t.Fatalf("ResolveIngestKey(custom) = %v, %v", got, err)
	}
	// The slug is not a key unless it was made the key, and keys are case sensitive.
	isErr(t, "slug of an inbox with its own key", func() error { _, err := s.ResolveIngestKey(ctx, "keys-custom"); return err }(), inbox.ErrNotFound)
	isErr(t, "key in the wrong case", func() error { _, err := s.ResolveIngestKey(ctx, "zq_9-mixed"); return err }(), inbox.ErrNotFound)

	// A key in use cannot be taken, by create or by update, and a failed attempt changes nothing.
	isErr(t, "duplicate key", s.CreateInbox(ctx, mkInbox("keys-dup", func(in *inbox.Inbox) { in.IngestKey = "Zq_9-Mixed" })), inbox.ErrConflict)
	taken := *plain
	taken.IngestKey = "Zq_9-Mixed"
	isErr(t, "update to a taken key", s.UpdateInbox(ctx, &taken), inbox.ErrConflict)
	if again, err := s.ResolveIngestKey(ctx, "keys-plain"); err != nil || again.ID != plain.ID {
		t.Errorf("a refused update changed the key: %v, %v", again, err)
	}
	// An update has to carry the key, as GetInbox returned it.
	blank := *plain
	blank.IngestKey = ""
	isErr(t, "update with no key", s.UpdateInbox(ctx, &blank), inbox.ErrInvalid)

	// Rotating the key moves the inbox; the old key stops working.
	moved := *plain
	moved.IngestKey = "fresh-key-123"
	if err := s.UpdateInbox(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if moved.IngestKey != "fresh-key-123" {
		t.Errorf("UpdateInbox returned key %q", moved.IngestKey)
	}
	if r, err := s.ResolveIngestKey(ctx, "fresh-key-123"); err != nil || r.ID != plain.ID {
		t.Errorf("new key: %v, %v", r, err)
	}
	isErr(t, "old key", func() error { _, err := s.ResolveIngestKey(ctx, "keys-plain"); return err }(), inbox.ErrNotFound)
	if r, err := s.GetInboxBySlug(ctx, "keys-plain"); err != nil || r.IngestKey != "fresh-key-123" {
		t.Errorf("by slug after rotation: %v, %v", r, err)
	}

	for name, key := range map[string]string{"too short": "ab", "too long": strings.Repeat("k", 65), "slash": "a/b", "dot": "a.b", "space": "a b", "non-ascii": "clé-clé"} {
		isErr(t, "ingest key "+name, s.CreateInbox(ctx, mkInbox("keys-bad", func(in *inbox.Inbox) { in.IngestKey = key })), inbox.ErrInvalid)
	}
}

func testCreateInboxValidation(t *testing.T, s inbox.Store, _ *Clock) {
	bad := map[string]func(*inbox.Inbox){
		"slug uppercase":       func(in *inbox.Inbox) { in.Slug = "Alpha" },
		"slug too short":       func(in *inbox.Inbox) { in.Slug = "ab" },
		"slug too long":        func(in *inbox.Inbox) { in.Slug = strings.Repeat("a", 65) },
		"slug with slash":      func(in *inbox.Inbox) { in.Slug = "a/b" },
		"slug with underscore": func(in *inbox.Inbox) { in.Slug = "a_b" },
		"empty name":           func(in *inbox.Inbox) { in.Name = "" },
		"name too long":        func(in *inbox.Inbox) { in.Name = strings.Repeat("é", 201) },
		"name invalid utf-8":   func(in *inbox.Inbox) { in.Name = "bad\xffname" },
		"empty verifier":       func(in *inbox.Inbox) { in.Verifier = "" },
		"verifier uppercase":   func(in *inbox.Inbox) { in.Verifier = "Stripe" },
		"verifier with space":  func(in *inbox.Inbox) { in.Verifier = "my verifier" },
		"bad on_verify_fail":   func(in *inbox.Inbox) { in.OnVerifyFail = "ignore" },
		"config not json":      func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`{nope`) },
		"config not object":    func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`[1,2]`) },
		"config null":          func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`null`) },
		"body limit negative":  func(in *inbox.Inbox) { in.MaxBodyBytes = -1 },
		"body limit too big":   func(in *inbox.Inbox) { in.MaxBodyBytes = inbox.MaxBodyBytesCeiling + 1 },
		"retention days":       func(in *inbox.Inbox) { in.RetentionDays = -1 },
		"retention count":      func(in *inbox.Inbox) { in.RetentionMaxCount = -1 },
		"pointer no slash":     func(in *inbox.Inbox) { in.EventTypePointer = "type" },
		"pointer bad escape":   func(in *inbox.Inbox) { in.EventTypePointer = "/a~2b" },
		"pointer trailing ~":   func(in *inbox.Inbox) { in.EventTypePointer = "/a~" },
	}
	for name, mod := range bad {
		in := mkInbox("valid-slug", mod)
		isErr(t, name, s.CreateInbox(t.Context(), in), inbox.ErrInvalid)
	}
	// None of the rejected attempts may have left anything behind.
	got, err := s.ListInboxes(t.Context(), inbox.ListParams{})
	if err != nil || len(got) != 0 {
		t.Errorf("rejected creates left inboxes behind: %v, %v", got, err)
	}
	// Boundary values, and verifiers an embedder registered, are valid.
	for _, slug := range []string{"abc", strings.Repeat("z", 64), "a-1-b"} {
		createInbox(t, s, slug)
	}
	createInbox(t, s, "ceiling", func(in *inbox.Inbox) { in.MaxBodyBytes = inbox.MaxBodyBytesCeiling })
	createInbox(t, s, "custom-verifier", func(in *inbox.Inbox) { in.Verifier = "acme-signature-v2" })
	createInbox(t, s, "wide-name", func(in *inbox.Inbox) { in.Name = strings.Repeat("é", 200) })
	createInbox(t, s, "pointer-ok", func(in *inbox.Inbox) { in.EventTypePointer = "/a~0b~1c/0" })
}

func testInboxAliasing(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "alias", func(in *inbox.Inbox) {
		in.VerifierSecret = []byte("secret")
		in.VerifierConfig = json.RawMessage(`{"a":1}`)
	})
	first, err := s.GetInbox(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	first.VerifierSecret[0] = 'X'
	first.VerifierConfig[0] = '['
	second, err := s.GetInbox(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(second.VerifierSecret) != "secret" ||
		!jsonEqual(second.VerifierConfig, json.RawMessage(`{"a":1}`)) {
		t.Error("mutating a returned inbox changed the stored one")
	}
}

func testInboxSecretsRedacted(t *testing.T, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "redacted", func(in *inbox.Inbox) {
		in.VerifierSecret = []byte("sealed-secret-marker")
	})
	got, err := s.GetInbox(t.Context(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	js, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for what, out := range map[string]string{
		"json":   string(js),
		"%v":     fmt.Sprintf("%v", got),
		"%+v":    fmt.Sprintf("%+v", *got),
		"%#v":    fmt.Sprintf("%#v", *got),
		"fields": fmt.Sprint(got.VerifierSecret),
	} {
		if strings.Contains(out, "sealed-secret-marker") {
			t.Errorf("%s output leaks a secret: %s", what, out)
		}
		if strings.Contains(out, fmt.Sprintf("%d", []byte("sealed-secret-marker"))) {
			t.Errorf("%s output leaks secret bytes: %s", what, out)
		}
	}
}

func testUpdateInbox(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "update-me")
	appendN(t, s, in.ID, 3, nil)
	created := in.CreatedAt

	clk.Advance(time.Hour)
	upd := *in
	upd.Slug = "tampered-slug" // immutable
	upd.LastSeq = 12345        // not writable
	upd.CreatedAt = baseTime   // not writable
	upd.Name = "Renamed"
	upd.Verifier = "hmac"
	upd.VerifierConfig = json.RawMessage(`{"header":"X-Hub"}`)
	upd.VerifierSecret = []byte("new-sealed-secret")
	upd.OnVerifyFail = inbox.OnVerifyFailStore
	upd.MaxBodyBytes = 2048
	upd.RetentionDays = 30
	upd.RetentionMaxCount = 500
	upd.EventTypePointer = "/event/type"
	upd.Disabled = true
	if err := s.UpdateInbox(ctx, &upd); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetInbox(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	sameInbox(t, "after update (refreshed value vs stored)", &upd, got)
	if got.Slug != "update-me" || got.LastSeq != 3 || !got.CreatedAt.Equal(created) || got.Version != 2 {
		t.Errorf("slug=%q lastSeq=%d createdAt=%v version=%d", got.Slug, got.LastSeq, got.CreatedAt, got.Version)
	}
	if !got.UpdatedAt.Equal(baseTime.Add(time.Hour)) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, baseTime.Add(time.Hour))
	}
	if got.Name != "Renamed" || got.Verifier != "hmac" || string(got.VerifierSecret) != "new-sealed-secret" ||
		got.OnVerifyFail != inbox.OnVerifyFailStore || got.MaxBodyBytes != 2048 || got.RetentionDays != 30 ||
		got.RetentionMaxCount != 500 || got.EventTypePointer != "/event/type" || !got.Disabled ||
		!jsonEqual(got.VerifierConfig, json.RawMessage(`{"header":"X-Hub"}`)) {
		t.Errorf("mutable fields not updated: %#v", *got)
	}
	if _, err := s.GetInboxBySlug(ctx, "tampered-slug"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("slug was changed by update: %v", err)
	}

	// The sequence counter carries on from where it was.
	if m := appendMsg(t, s, in.ID); m.Seq != 4 {
		t.Errorf("Seq after update = %d, want 4", m.Seq)
	}

	// Invalid updates are rejected and change nothing.
	bad := *got
	bad.Verifier = "NOT VALID"
	bad.Name = "should not stick"
	isErr(t, "invalid update", s.UpdateInbox(ctx, &bad), inbox.ErrInvalid)
	after, _ := s.GetInbox(ctx, in.ID)
	if after.Name != "Renamed" || after.Verifier != "hmac" || after.Version != 2 {
		t.Errorf("rejected update changed the inbox: %#v", *after)
	}

	gone := *got
	gone.ID += 1000
	isErr(t, "update missing", s.UpdateInbox(ctx, &gone), inbox.ErrNotFound)
}

func testUpdateVersioning(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "versioned")

	// Two admins read the same version; the second to write must lose.
	adminA, adminB := *in, *in
	adminA.Name = "from admin A"
	adminB.Name = "from admin B"
	if err := s.UpdateInbox(ctx, &adminA); err != nil {
		t.Fatal(err)
	}
	isErr(t, "stale update", s.UpdateInbox(ctx, &adminB), inbox.ErrConflict)

	got, _ := s.GetInbox(ctx, in.ID)
	if got.Name != "from admin A" || got.Version != 2 {
		t.Errorf("the losing update leaked through: %#v", *got)
	}

	// Re-reading and retrying succeeds, and versions keep counting.
	adminB = *got
	adminB.Name = "from admin B"
	if err := s.UpdateInbox(ctx, &adminB); err != nil {
		t.Fatal(err)
	}
	if adminB.Version != 3 {
		t.Errorf("Version = %d, want 3", adminB.Version)
	}

	// Updating the same struct again works because the call refreshed it.
	adminB.Name = "again"
	if err := s.UpdateInbox(ctx, &adminB); err != nil || adminB.Version != 4 {
		t.Errorf("sequential updates: version %d, err %v", adminB.Version, err)
	}

	// Concurrent updates from one base version: exactly one wins.
	base, _ := s.GetInbox(ctx, in.ID)
	const n = 10
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			u := *base
			u.Name = fmt.Sprintf("racer-%d", i)
			results <- s.UpdateInbox(ctx, &u)
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, inbox.ErrConflict):
			t.Errorf("unexpected error from a racing update: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d concurrent updates from the same version succeeded, want exactly 1", wins)
	}
}

func testDeleteInbox(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	a := createInbox(t, s, "doomed")
	b := createInbox(t, s, "survivor")
	appendN(t, s, a.ID, 5, nil)
	appendN(t, s, b.ID, 3, nil)

	if err := s.DeleteInbox(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	isErr(t, "GetInbox after delete", func() error { _, err := s.GetInbox(ctx, a.ID); return err }(), inbox.ErrNotFound)
	isErr(t, "ResolveIngestKey after delete", func() error { _, err := s.ResolveIngestKey(ctx, "doomed"); return err }(), inbox.ErrNotFound)
	isErr(t, "Query after delete", func() error { _, err := s.Query(ctx, a.ID, inbox.Query{}); return err }(), inbox.ErrNotFound)
	isErr(t, "Append after delete", func() error { _, _, err := s.Append(ctx, a.ID, nm()); return err }(), inbox.ErrNotFound)
	isErr(t, "second delete", s.DeleteInbox(ctx, a.ID), inbox.ErrNotFound)

	if st, err := s.Stats(ctx, b.ID); err != nil || st.MessageCount != 3 {
		t.Errorf("deleting one inbox disturbed another: %+v, %v", st, err)
	}

	// The slug can be reused and the new inbox starts empty.
	again := createInbox(t, s, "doomed")
	if again.ID == a.ID {
		t.Error("a deleted inbox's id was reused")
	}
	p := query(t, s, again.ID, inbox.Query{})
	if len(p.Messages) != 0 || p.LatestSeq != 0 {
		t.Errorf("new inbox inherited data: %+v", p)
	}
	if m := appendMsg(t, s, again.ID); m.Seq != 1 {
		t.Errorf("new inbox started at seq %d, want 1", m.Seq)
	}
}

func testListInboxes(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	if got, err := s.ListInboxes(ctx, inbox.ListParams{}); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v", got, err)
	}
	var ids []inbox.ID
	for i := range 7 {
		ids = append(ids, createInbox(t, s, fmt.Sprintf("list-%d", i)).ID)
	}

	all, err := s.ListInboxes(ctx, inbox.ListParams{})
	if err != nil || len(all) != 7 {
		t.Fatalf("default list: %d inboxes, %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("not in ascending id order: %d then %d", all[i-1].ID, all[i].ID)
		}
	}

	// Page through with a small limit.
	var paged []inbox.ID
	var after inbox.ID
	for range 10 {
		page, err := s.ListInboxes(ctx, inbox.ListParams{AfterID: after, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 3 {
			t.Fatalf("page of %d exceeds limit 3", len(page))
		}
		for _, in := range page {
			paged = append(paged, in.ID)
			after = in.ID
		}
	}
	if !reflect.DeepEqual(paged, ids) {
		t.Errorf("paging saw %v, want %v", paged, ids)
	}
}

func testPing(t *testing.T, s inbox.Store, _ *Clock) {
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("Ping on a healthy store: %v", err)
	}
}

// ---- append ----

func testAppendBasics(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "basics")
	for want := int64(1); want <= 3; want++ {
		m := appendMsg(t, s, in.ID)
		if m.Seq != want || m.InboxID != in.ID {
			t.Errorf("Seq = %d (inbox %d), want %d (inbox %d)", m.Seq, m.InboxID, want, in.ID)
		}
	}
	if seq, err := s.LatestSeq(ctx, in.ID); err != nil || seq != 3 {
		t.Errorf("LatestSeq = %d, %v; want 3", seq, err)
	}
	if got, _ := s.GetInbox(ctx, in.ID); got.LastSeq != 3 {
		t.Errorf("Inbox.LastSeq = %d, want 3", got.LastSeq)
	}

	_, _, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) { m.Method = "" }))
	isErr(t, "missing method", err, inbox.ErrInvalid)
	if seq, _ := s.LatestSeq(ctx, in.ID); seq != 3 {
		t.Errorf("a rejected append consumed a sequence number: LatestSeq = %d", seq)
	}
	_, _, err = s.Append(ctx, in.ID+1000, nm())
	isErr(t, "unknown inbox", err, inbox.ErrNotFound)

	// A disabled inbox still stores what the caller appends; refusing ingest
	// is the server's job. Reads work either way.
	off := *in
	off.Disabled = true
	if err := s.UpdateInbox(ctx, &off); err != nil {
		t.Fatal(err)
	}
	if page := query(t, s, in.ID, inbox.Query{}); len(page.Messages) != 3 {
		t.Errorf("a disabled inbox should stay readable, got %d messages", len(page.Messages))
	}
}

func testReceivedAt(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "received-at")

	// The Store's clock is used, truncated to microseconds, in UTC.
	denver := time.FixedZone("MST", -7*3600)
	clk.Set(baseTime.Add(1234567891 * time.Nanosecond).In(denver))
	m := appendMsg(t, s, in.ID)
	want := baseTime.Add(1234567 * time.Microsecond)
	if !m.ReceivedAt.Equal(want) || m.ReceivedAt.Location() != time.UTC {
		t.Errorf("ReceivedAt = %v (%v), want %v in UTC", m.ReceivedAt, m.ReceivedAt.Location(), want)
	}
	got, err := s.Get(ctx, in.ID, m.Seq)
	if err != nil || !got.ReceivedAt.Equal(want) || got.ReceivedAt.Location() != time.UTC {
		t.Errorf("Get returned ReceivedAt %v, %v; want exactly %v", got.ReceivedAt, err, want)
	}

	// If the clock steps backwards, ReceivedAt still never decreases with Seq.
	first := appendAt(t, s, clk, in.ID, baseTime.Add(10*time.Hour))
	second := appendAt(t, s, clk, in.ID, baseTime.Add(2*time.Hour)) // clock went back
	third := appendAt(t, s, clk, in.ID, baseTime.Add(11*time.Hour))
	if second.ReceivedAt.Before(first.ReceivedAt) || third.ReceivedAt.Before(second.ReceivedAt) {
		t.Errorf("ReceivedAt decreased: %v, %v, %v", first.ReceivedAt, second.ReceivedAt, third.ReceivedAt)
	}
	if !third.ReceivedAt.Equal(baseTime.Add(11 * time.Hour)) {
		t.Errorf("a later clock reading should be used as is, got %v", third.ReceivedAt)
	}
}

func testAppendDedupe(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	a := createInbox(t, s, "dedupe-a")
	b := createInbox(t, s, "dedupe-b")

	first := appendMsg(t, s, a.ID, func(m *inbox.NewMessage) {
		m.DeliveryID = "evt_1"
		m.Body = []byte("original")
	})
	dup, created, err := s.Append(ctx, a.ID, nm(func(m *inbox.NewMessage) {
		m.DeliveryID = "evt_1"
		m.Body = []byte("replay with different content")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if created || dup.Seq != first.Seq || string(dup.Body) != "original" {
		t.Errorf("duplicate: created=%v seq=%d body=%q; want existing message seq %d", created, dup.Seq, dup.Body, first.Seq)
	}
	if got, _ := s.GetInbox(ctx, a.ID); got.LastSeq != 1 {
		t.Errorf("a duplicate consumed a sequence number: LastSeq = %d", got.LastSeq)
	}
	if next := appendMsg(t, s, a.ID); next.Seq != 2 {
		t.Errorf("next Seq = %d, want 2", next.Seq)
	}

	// The same delivery id in another inbox is a different delivery.
	other := appendMsg(t, s, b.ID, func(m *inbox.NewMessage) { m.DeliveryID = "evt_1" })
	if other.Seq != 1 {
		t.Errorf("other inbox Seq = %d, want 1", other.Seq)
	}

	// Without a delivery id, nothing dedupes, even identical content.
	x := appendMsg(t, s, b.ID)
	y := appendMsg(t, s, b.ID)
	if x.Seq == y.Seq {
		t.Error("messages without a delivery id were deduplicated")
	}
	if got := query(t, s, b.ID, inbox.Query{DeliveryID: ""}); len(got.Messages) != 3 {
		t.Errorf("empty DeliveryID filter should match everything, got %d messages", len(got.Messages))
	}
}

// What deduplication compares is the dedupe key, which is the delivery id unless
// the caller says otherwise: a caller whose ids are not signed makes it the id
// and a hash of the body.
func testAppendDedupeKey(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "dedupe-key")
	first := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) {
		m.DeliveryID, m.DedupeKey, m.Body = "d-1", "d-1#aaa", []byte("first")
	})
	// Same id, different key: a different delivery.
	second := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) {
		m.DeliveryID, m.DedupeKey, m.Body = "d-1", "d-1#bbb", []byte("second")
	})
	if first.Seq == second.Seq {
		t.Error("deliveries with the same id and different keys were deduplicated")
	}
	if second.DeliveryID != "d-1" {
		t.Errorf("DeliveryID = %q; the id is kept for display whatever the key", second.DeliveryID)
	}
	// Same key: a duplicate, even with another delivery id.
	dup, created, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) {
		m.DeliveryID, m.DedupeKey, m.Body = "other-id", "d-1#aaa", []byte("replay")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if created || dup.Seq != first.Seq {
		t.Errorf("same key: created=%v seq=%d, want the first message (seq %d)", created, dup.Seq, first.Seq)
	}
	// Without a key, the id is the key.
	a := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) { m.DeliveryID = "plain" })
	if _, created, _ := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) { m.DeliveryID = "plain" })); created {
		t.Errorf("a repeated id with no key was stored again (first was seq %d)", a.Seq)
	}
}

// A forged, unverified delivery that claims a real delivery id must neither be
// deduplicated nor cause the genuine delivery to be dropped.
func testDedupeIgnoresUnverified(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "forgery", func(in *inbox.Inbox) { in.OnVerifyFail = inbox.OnVerifyFailStore })
	unverified := func(m *inbox.NewMessage) {
		m.Verified = false
		m.VerifyError = "bad signature"
		m.DeliveryID = "evt_real"
	}

	forged1 := appendMsg(t, s, in.ID, unverified)
	forged2 := appendMsg(t, s, in.ID, unverified)
	if forged1.Seq == forged2.Seq {
		t.Error("two unverified messages with the same delivery id were deduplicated")
	}

	genuine, created, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) {
		m.DeliveryID = "evt_real"
		m.Body = []byte("the genuine delivery")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !created || genuine.Seq != 3 || string(genuine.Body) != "the genuine delivery" {
		t.Fatalf("the genuine delivery was suppressed by a forgery: created=%v seq=%d", created, genuine.Seq)
	}

	// A replay of the genuine delivery is a duplicate of the verified one, not
	// of the forgeries.
	replay, created, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) { m.DeliveryID = "evt_real" }))
	if err != nil || created || replay.Seq != genuine.Seq {
		t.Errorf("replay: created=%v seq=%d err=%v; want a duplicate of seq %d", created, replay.Seq, err, genuine.Seq)
	}

	// An unverified message that arrives after a verified one is still stored.
	late := appendMsg(t, s, in.ID, unverified)
	if late.Seq != 4 {
		t.Errorf("late forgery got seq %d, want 4", late.Seq)
	}
	if st, _ := s.Stats(ctx, in.ID); st.MessageCount != 4 {
		t.Errorf("MessageCount = %d, want 4", st.MessageCount)
	}
}

func testAppendDedupeConcurrent(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "dedupe-race")
	const n = 20
	type result struct {
		seq     int64
		created bool
		err     error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			m, created, err := s.Append(ctx, in.ID, nm(func(m *inbox.NewMessage) { m.DeliveryID = "same-delivery" }))
			r := result{created: created, err: err}
			if m != nil {
				r.seq = m.Seq
			}
			results <- r
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("a racing duplicate returned an error (never ErrConflict): %v", r.err)
		}
		if r.created {
			created++
		}
		if r.seq != 1 {
			t.Errorf("a racing duplicate got seq %d, want 1", r.seq)
		}
	}
	if created != 1 {
		t.Errorf("%d appends reported created=true, want exactly 1", created)
	}
	if seq, _ := s.LatestSeq(ctx, in.ID); seq != 1 {
		t.Errorf("LatestSeq = %d, want 1", seq)
	}
}

func testRoundTrip(t *testing.T, s inbox.Store, clk *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "roundtrip")

	big := make([]byte, 1<<20+13)
	for i := range big {
		big[i] = byte(i*7 + i>>8)
	}
	bodies := map[string][]byte{
		"nil":               nil,
		"empty":             {},
		"json":              []byte(`{"id":"evt_1","amount":100}`),
		"json with spacing": []byte(" {\"a\":1,\"a\":2}\r\n\t"),
		"invalid utf-8":     {0xff, 0xfe, 0x80, 0xc0, 0xaf},
		"nul bytes":         {0, 0, 'a', 0, 'b', 0},
		"every byte":        allBytes(),
		"crlf":              []byte("line1\r\nline2\n\rline3"),
		"over one MiB":      big,
	}
	for name, body := range bodies {
		sent := bytes.Clone(body)
		msg := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) { m.Body = sent })
		if msg.Size != int64(len(body)) || !bytes.Equal(msg.Body, body) || msg.Body == nil {
			t.Errorf("%s: Append returned size=%d len=%d nil=%v", name, msg.Size, len(msg.Body), msg.Body == nil)
		}
		got, err := s.Get(ctx, in.ID, msg.Seq)
		if err != nil {
			t.Fatalf("%s: Get: %v", name, err)
		}
		if !bytes.Equal(got.Body, body) || got.Body == nil || got.Size != int64(len(body)) {
			t.Errorf("%s: Get returned a different body (len %d, size %d, want %d, nil=%v)", name, len(got.Body), got.Size, len(body), got.Body == nil)
		}
		viaQuery := query(t, s, in.ID, inbox.Query{After: ptr(msg.Seq - 1), Limit: 1})
		if len(viaQuery.Messages) != 1 || !bytes.Equal(viaQuery.Messages[0].Body, body) || viaQuery.Messages[0].Body == nil {
			t.Errorf("%s: Query returned a different body", name)
		}
	}

	// Every other field survives, too.
	clk.Set(baseTime.Add(1234567 * time.Microsecond))
	want := &inbox.NewMessage{
		Method:      "PUT",
		PathSuffix:  "orders/42/events",
		Query:       "x=1&y=%20z&x=2",
		Headers:     http.Header{"Content-Type": {"application/json; charset=utf-8"}, "X-Multi": {"first", "second", "third"}, "X-Empty": {""}},
		ContentType: "application/json; charset=utf-8",
		Body:        []byte(`{"ok":true}`),
		Verified:    false,
		VerifyError: "signature mismatch",
		DeliveryID:  "evt_roundtrip",
		EventType:   "invoice.paid",
		SourceIP:    "203.0.113.7",
	}
	stored, _, err := s.Append(ctx, in.ID, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, in.ID, stored.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if got.InboxID != in.ID || got.Seq != stored.Seq || !got.ReceivedAt.Equal(baseTime.Add(1234567*time.Microsecond)) ||
		got.Method != want.Method || got.PathSuffix != want.PathSuffix || got.Query != want.Query ||
		got.ContentType != want.ContentType || got.Verified || got.VerifyError != want.VerifyError ||
		got.DeliveryID != want.DeliveryID || got.EventType != want.EventType || got.SourceIP != want.SourceIP {
		t.Errorf("fields differ:\n got: %+v\nwant: %+v", *got, *want)
	}
	if !reflect.DeepEqual(got.Headers, want.Headers) {
		t.Errorf("headers = %v, want %v", got.Headers, want.Headers)
	}

	// Header names are canonicalized.
	canon := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) {
		m.Headers = http.Header{"x-lower-case": {"v"}, "X-UPPER": {"w"}}
	})
	if v := canon.Headers["X-Lower-Case"]; len(v) != 1 || v[0] != "v" {
		t.Errorf("headers not canonicalized: %v", canon.Headers)
	}
	if v := canon.Headers["X-Upper"]; len(v) != 1 || v[0] != "w" {
		t.Errorf("headers not canonicalized: %v", canon.Headers)
	}

	// Nil headers are fine and come back as a usable, non-nil map.
	none := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) { m.Headers = nil })
	reread, _ := s.Get(ctx, in.ID, none.Seq)
	if none.Headers == nil || reread.Headers == nil || len(reread.Headers) != 0 {
		t.Errorf("headers for a message with none: %#v / %#v, want non-nil and empty", none.Headers, reread.Headers)
	}
}

func testMessageAliasing(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "aliasing")

	sentBody := []byte("original body")
	sentHeaders := http.Header{"X-Keep": {"original"}}
	msg := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) {
		m.Body = sentBody
		m.Headers = sentHeaders
	})
	// Neither the caller's slices nor the returned message may share memory
	// with what is stored.
	sentBody[0] = 'X'
	sentHeaders["X-Keep"][0] = "mutated"
	msg.Body[1] = 'Y'
	msg.Headers["X-Keep"][0] = "mutated-too"

	got, err := s.Get(ctx, in.ID, msg.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != "original body" || got.Headers.Get("X-Keep") != "original" {
		t.Errorf("stored message was altered through an alias: body=%q header=%q", got.Body, got.Headers.Get("X-Keep"))
	}
	got.Body[2] = 'Z'
	again, _ := s.Get(ctx, in.ID, msg.Seq)
	if string(again.Body) != "original body" {
		t.Errorf("mutating a Get result altered the store: %q", again.Body)
	}
}

func testHeaderRedaction(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "redaction")
	msg := appendMsg(t, s, in.ID, func(m *inbox.NewMessage) {
		m.Headers = http.Header{
			"Authorization":       {"Bearer super-secret-token"},
			"authorization":       {"Bearer second-secret"},
			"COOKIE":              {"session=hunter2"},
			"Proxy-Authorization": {"Basic cHJveHk6c2VjcmV0"},
			"X-Keep":              {"visible"},
		}
	})
	checkRedacted(t, "Append result", msg.Headers)
	got, err := s.Get(ctx, in.ID, msg.Seq)
	if err != nil {
		t.Fatal(err)
	}
	checkRedacted(t, "Get", got.Headers)
	p := query(t, s, in.ID, inbox.Query{})
	checkRedacted(t, "Query", p.Messages[0].Headers)
}

func checkRedacted(t *testing.T, what string, h http.Header) {
	t.Helper()
	for k, vs := range h {
		for _, redacted := range inbox.AlwaysRedactedHeaders {
			if strings.EqualFold(k, redacted) {
				t.Errorf("%s: header %q was persisted", what, k)
			}
		}
		for _, v := range vs {
			for _, secret := range []string{"super-secret-token", "second-secret", "hunter2", "cHJveHk6c2VjcmV0"} {
				if strings.Contains(v, secret) {
					t.Errorf("%s: secret value leaked in header %q", what, k)
				}
			}
		}
	}
	if h.Get("X-Keep") != "visible" {
		t.Errorf("%s: ordinary header was dropped: %v", what, h)
	}
}

func testGetAndStats(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	a := createInbox(t, s, "stats-a")
	b := createInbox(t, s, "stats-b")

	st, err := s.Stats(ctx, a.ID)
	if err != nil || *st != (inbox.InboxStats{}) {
		t.Errorf("empty inbox stats = %+v, %v", st, err)
	}
	if seq, err := s.LatestSeq(ctx, a.ID); err != nil || seq != 0 {
		t.Errorf("empty LatestSeq = %d, %v", seq, err)
	}

	appendN(t, s, a.ID, 5, nil)
	appendN(t, s, b.ID, 3, nil)
	st, err = s.Stats(ctx, a.ID)
	if err != nil || *st != (inbox.InboxStats{MessageCount: 5, OldestSeq: 1, LatestSeq: 5}) {
		t.Errorf("stats = %+v, %v", st, err)
	}

	if m, err := s.Get(ctx, a.ID, 2); err != nil || m.Seq != 2 || string(m.Body) != "message-2" {
		t.Errorf("Get(2) = %+v, %v", m, err)
	}
	for _, seq := range []int64{0, -1, 6, 1 << 40} {
		_, err := s.Get(ctx, a.ID, seq)
		isErr(t, fmt.Sprintf("Get(%d)", seq), err, inbox.ErrNotFound)
	}
	// Inbox b has a seq 3; an empty inbox must not answer for it.
	if _, err := s.Get(ctx, b.ID, 3); err != nil {
		t.Errorf("Get(b, 3): %v", err)
	}
	_, err = s.Get(ctx, createInbox(t, s, "stats-c").ID, 3)
	isErr(t, "Get of another inbox's seq", err, inbox.ErrNotFound)
}

func testMissingInbox(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	const missing inbox.ID = 1 << 40
	calls := map[string]func() error{
		"Append":    func() error { _, _, err := s.Append(ctx, missing, nm()); return err },
		"Query":     func() error { _, err := s.Query(ctx, missing, inbox.Query{}); return err },
		"Get":       func() error { _, err := s.Get(ctx, missing, 1); return err },
		"LatestSeq": func() error { _, err := s.LatestSeq(ctx, missing); return err },
		"Stats":     func() error { _, err := s.Stats(ctx, missing); return err },
		"Delete":    func() error { return s.DeleteInbox(ctx, missing) },
	}
	for name, call := range calls {
		isErr(t, name, call(), inbox.ErrNotFound)
	}
}

func testCancelledContext(t *testing.T, s inbox.Store, _ *Clock) {
	in := createInbox(t, s, "cancelled")
	appendN(t, s, in.ID, 2, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func() error{
		"CreateInbox":    func() error { return s.CreateInbox(ctx, mkInbox("never-created")) },
		"UpdateInbox":    func() error { u := *in; u.Name = "never"; return s.UpdateInbox(ctx, &u) },
		"Append":         func() error { _, _, err := s.Append(ctx, in.ID, nm()); return err },
		"Query":          func() error { _, err := s.Query(ctx, in.ID, inbox.Query{}); return err },
		"Get":            func() error { _, err := s.Get(ctx, in.ID, 1); return err },
		"LatestSeq":      func() error { _, err := s.LatestSeq(ctx, in.ID); return err },
		"Stats":          func() error { _, err := s.Stats(ctx, in.ID); return err },
		"GetTokenByHash": func() error { _, err := s.GetTokenByHash(ctx, tokenHash("x")); return err },
		"ListTokens":     func() error { _, err := s.ListTokens(ctx, inbox.TokenListParams{}); return err },
		"Prune":          func() error { _, err := s.Prune(ctx, baseTime); return err },
		"Ping":           func() error { return s.Ping(ctx) },
	}
	for name, call := range calls {
		isErr(t, name, call(), context.Canceled)
	}
	// A cancelled operation leaves no trace.
	if seq, _ := s.LatestSeq(t.Context(), in.ID); seq != 2 {
		t.Errorf("a cancelled Append changed LatestSeq to %d", seq)
	}
	if _, err := s.GetInboxBySlug(t.Context(), "never-created"); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("a cancelled CreateInbox left an inbox behind: %v", err)
	}
	if got, _ := s.GetInbox(t.Context(), in.ID); got.Name == "never" || got.Version != 1 {
		t.Errorf("a cancelled UpdateInbox changed the inbox: %#v", *got)
	}
}

func mkToken(label string, mods ...func(*inbox.Token)) *inbox.Token {
	t := &inbox.Token{Name: label, Hash: tokenHash(label), AllInboxes: true}
	for _, m := range mods {
		m(t)
	}
	return t
}

func scoped(ids ...inbox.ID) func(*inbox.Token) {
	return func(t *inbox.Token) { t.AllInboxes, t.InboxIDs = false, ids }
}

func testTokens(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	a := createInbox(t, s, "tok-a")
	b := createInbox(t, s, "tok-b")

	one := mkToken("one", scoped(a.ID))
	multi := mkToken("multi", scoped(b.ID, a.ID))
	every := mkToken("every")
	for _, tk := range []*inbox.Token{one, multi, every} {
		if err := s.CreateToken(ctx, tk); err != nil {
			t.Fatalf("CreateToken %s: %v", tk.Name, err)
		}
		if tk.ID == 0 || tk.CreatedAt.IsZero() {
			t.Errorf("CreateToken %s left id/created unset: %+v", tk.Name, tk)
		}
	}

	got, err := s.GetTokenByHash(ctx, tokenHash("multi"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != multi.ID || got.Name != "multi" || got.AllInboxes || len(got.InboxIDs) != 2 ||
		got.InboxIDs[0] > got.InboxIDs[1] || !bytes.Equal(got.Hash, tokenHash("multi")) {
		t.Errorf("GetTokenByHash(multi) = %+v", got)
	}
	if got, err := s.GetTokenByHash(ctx, tokenHash("every")); err != nil || !got.AllInboxes || len(got.InboxIDs) != 0 {
		t.Errorf("GetTokenByHash(every) = %+v, %v", got, err)
	}
	_, err = s.GetTokenByHash(ctx, tokenHash("nobody"))
	isErr(t, "unknown hash", err, inbox.ErrNotFound)

	// Invalid tokens, an unknown inbox and a taken hash.
	isErr(t, "invalid", s.CreateToken(ctx, mkToken("bad", func(t *inbox.Token) { t.Name = "" })), inbox.ErrInvalid)
	isErr(t, "unknown inbox", s.CreateToken(ctx, mkToken("ghost", scoped(a.ID, 999999))), inbox.ErrNotFound)
	isErr(t, "taken hash", s.CreateToken(ctx, mkToken("dup", func(t *inbox.Token) { t.Hash = tokenHash("one") })), inbox.ErrConflict)
	if _, err := s.GetTokenByHash(ctx, tokenHash("ghost")); !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("a refused token was stored: %v", err)
	}

	// Listing: by id, paged, and filtered to the tokens that can reach an inbox.
	names := func(p inbox.TokenListParams) []string {
		tks, err := s.ListTokens(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tk := range tks {
			out = append(out, tk.Name)
		}
		return out
	}
	if got := names(inbox.TokenListParams{}); !reflect.DeepEqual(got, []string{"one", "multi", "every"}) {
		t.Errorf("ListTokens = %v", got)
	}
	if got := names(inbox.TokenListParams{AfterID: one.ID, Limit: 1}); !reflect.DeepEqual(got, []string{"multi"}) {
		t.Errorf("ListTokens page = %v", got)
	}
	if got := names(inbox.TokenListParams{InboxID: a.ID}); !reflect.DeepEqual(got, []string{"one", "multi", "every"}) {
		t.Errorf("ListTokens for a = %v", got)
	}
	if got := names(inbox.TokenListParams{InboxID: b.ID}); !reflect.DeepEqual(got, []string{"multi", "every"}) {
		t.Errorf("ListTokens for b = %v", got)
	}

	// Deleting a token revokes it, and frees its hash.
	if err := s.DeleteToken(ctx, one.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetTokenByHash(ctx, tokenHash("one"))
	isErr(t, "revoked", err, inbox.ErrNotFound)
	isErr(t, "second revoke", s.DeleteToken(ctx, one.ID), inbox.ErrNotFound)
	if err := s.CreateToken(ctx, mkToken("one", scoped(b.ID))); err != nil {
		t.Errorf("a revoked token's hash was not freed: %v", err)
	}
}

func testTokensOfADeletedInbox(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	a := createInbox(t, s, "gone-a")
	b := createInbox(t, s, "gone-b")
	for _, tk := range []*inbox.Token{
		mkToken("only-a", scoped(a.ID)),
		mkToken("a-and-b", scoped(a.ID, b.ID)),
		mkToken("everything"),
	} {
		if err := s.CreateToken(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteInbox(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetTokenByHash(ctx, tokenHash("only-a"))
	isErr(t, "token scoped to the deleted inbox only", err, inbox.ErrNotFound)
	if got, err := s.GetTokenByHash(ctx, tokenHash("a-and-b")); err != nil || got.AllInboxes || len(got.InboxIDs) != 1 || got.InboxIDs[0] != b.ID {
		t.Errorf("a-and-b = %+v, %v; want it narrowed to b", got, err)
	}
	if got, err := s.GetTokenByHash(ctx, tokenHash("everything")); err != nil || !got.AllInboxes {
		t.Errorf("an all-inboxes token was disturbed: %+v, %v", got, err)
	}
}

func testEpochs(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	a := createInbox(t, s, "epoch-a")
	b := createInbox(t, s, "epoch-b")
	if a.Epoch == "" || b.Epoch == "" || a.Epoch == b.Epoch {
		t.Fatalf("epochs of two inboxes: %q and %q; want two different, non-empty values", a.Epoch, b.Epoch)
	}
	for name, get := range map[string]func() (*inbox.Inbox, error){
		"GetInbox":         func() (*inbox.Inbox, error) { return s.GetInbox(ctx, a.ID) },
		"GetInboxBySlug":   func() (*inbox.Inbox, error) { return s.GetInboxBySlug(ctx, "epoch-a") },
		"ResolveIngestKey": func() (*inbox.Inbox, error) { return s.ResolveIngestKey(ctx, a.IngestKey) },
	} {
		if got, err := get(); err != nil || got.Epoch != a.Epoch {
			t.Errorf("%s: epoch %q, %v; want %q", name, got.Epoch, err, a.Epoch)
		}
	}

	// Neither messages nor edits change it.
	appendN(t, s, a.ID, 3, nil)
	got, _ := s.GetInbox(ctx, a.ID)
	got.Name = "renamed"
	got.Epoch = "set-by-the-caller"
	if err := s.UpdateInbox(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetInbox(ctx, a.ID); again.Epoch != a.Epoch {
		t.Errorf("an update changed the epoch to %q", again.Epoch)
	}

	// Creating an inbox ignores an epoch the caller supplies.
	c := mkInbox("epoch-c")
	c.Epoch = "chosen"
	if err := s.CreateInbox(ctx, c); err != nil || c.Epoch == "chosen" || c.Epoch == "" {
		t.Errorf("CreateInbox kept the caller's epoch: %q, %v", c.Epoch, err)
	}

	// The same slug, made again after a delete, is a new lifetime.
	if err := s.DeleteInbox(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	again := createInbox(t, s, "epoch-a")
	if again.Epoch == a.Epoch {
		t.Error("an inbox recreated under the same slug has the old epoch")
	}
}

func testUIDs(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	a := createInbox(t, s, "uid-a")
	b := createInbox(t, s, "uid-b")
	if !inbox.ValidUID(a.UID) || !inbox.ValidUID(b.UID) || a.UID == b.UID {
		t.Fatalf("UIDs of two inboxes: %q and %q; want two different, valid values", a.UID, b.UID)
	}
	for name, get := range map[string]func() (*inbox.Inbox, error){
		"GetInbox":         func() (*inbox.Inbox, error) { return s.GetInbox(ctx, a.ID) },
		"GetInboxBySlug":   func() (*inbox.Inbox, error) { return s.GetInboxBySlug(ctx, "uid-a") },
		"ResolveIngestKey": func() (*inbox.Inbox, error) { return s.ResolveIngestKey(ctx, a.IngestKey) },
	} {
		if got, err := get(); err != nil || got.UID != a.UID {
			t.Errorf("%s: uid %q, %v; want %q", name, got.UID, err, a.UID)
		}
	}
	listed, err := s.ListInboxes(ctx, inbox.ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, in := range listed {
		seen[in.Slug] = in.UID
	}
	if seen["uid-a"] != a.UID || seen["uid-b"] != b.UID {
		t.Errorf("ListInboxes: uids %v; want %q and %q", seen, a.UID, b.UID)
	}

	// An edit does not change it, whatever the caller puts in the field.
	got, _ := s.GetInbox(ctx, a.ID)
	got.Name = "renamed"
	got.UID = b.UID
	if err := s.UpdateInbox(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetInbox(ctx, a.ID); again.UID != a.UID {
		t.Errorf("an update changed the uid to %q", again.UID)
	}

	// A caller that must seal something to the inbox first may choose it.
	chosen, err := inbox.NewUID()
	if err != nil {
		t.Fatal(err)
	}
	c := mkInbox("uid-c")
	c.UID = chosen
	if err := s.CreateInbox(ctx, c); err != nil || c.UID != chosen {
		t.Errorf("CreateInbox with a uid: kept %q, %v; want %q", c.UID, err, chosen)
	}
	// Another inbox cannot have it, and one that is not valid is refused.
	d := mkInbox("uid-d")
	d.UID = chosen
	isErr(t, "a uid in use", s.CreateInbox(ctx, d), inbox.ErrConflict)
	e := mkInbox("uid-e")
	e.UID = "short"
	isErr(t, "an invalid uid", s.CreateInbox(ctx, e), inbox.ErrInvalid)

	// The same slug, made again after a delete, is a different inbox, and so has a
	// different uid: what was sealed to the old one must not open on the new one.
	if err := s.DeleteInbox(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	again := createInbox(t, s, "uid-a")
	if again.UID == a.UID {
		t.Error("an inbox recreated under the same slug has the old uid")
	}
}

func testOldestSeqInAPage(t *testing.T, s inbox.Store, _ *Clock) {
	ctx := t.Context()
	in := createInbox(t, s, "oldest-seq", func(in *inbox.Inbox) { in.RetentionMaxCount = 2 })
	if p := query(t, s, in.ID, inbox.Query{}); p.OldestSeq != 0 || p.LatestSeq != 0 {
		t.Errorf("an empty inbox: oldest %d, latest %d", p.OldestSeq, p.LatestSeq)
	}
	appendN(t, s, in.ID, 5, nil)
	if p := query(t, s, in.ID, inbox.Query{}); p.OldestSeq != 1 || p.LatestSeq != 5 {
		t.Errorf("before pruning: oldest %d, latest %d", p.OldestSeq, p.LatestSeq)
	}
	if _, err := s.Prune(ctx, baseTime); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]inbox.Query{
		"newest first":    {},
		"after a cursor":  {After: ptr(int64(4))},
		"a filter":        {Types: []string{"no-such-type"}},
		"past the newest": {After: ptr(int64(9))},
	} {
		if p := query(t, s, in.ID, q); p.OldestSeq != 4 || p.LatestSeq != 5 {
			t.Errorf("%s: oldest %d, latest %d; want 4 and 5 whatever the query", name, p.OldestSeq, p.LatestSeq)
		}
	}
	if st, _ := s.Stats(ctx, in.ID); st.OldestSeq != 4 || st.MessageCount != 2 {
		t.Errorf("stats = %+v", st)
	}
}
