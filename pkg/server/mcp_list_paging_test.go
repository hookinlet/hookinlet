// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hookinlet/hookinlet/pkg/auth/token"
	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// listPage is one list_inboxes result as an agent reads it.
type listPage struct {
	Inboxes   []inboxSummary `json:"inboxes"`
	Truncated bool           `json:"truncated"`
	NextAfter string         `json:"next_after"`
	raw       json.RawMessage
}

func (p listPage) slugs() []string {
	out := make([]string, 0, len(p.Inboxes))
	for _, s := range p.Inboxes {
		out = append(out, s.Slug)
	}
	return out
}

// listInboxesPage calls list_inboxes, passing after unless it is empty, and
// fails the test on a tool error.
func listInboxesPage(t *testing.T, cs *mcp.ClientSession, after string) listPage {
	t.Helper()
	var args map[string]any
	if after != "" {
		args = map[string]any{"after": after}
	}
	o := callTool(t, cs, "list_inboxes", args)
	if o.err != "" {
		t.Fatalf("list_inboxes(after=%q): tool error: %s", after, o.err)
	}
	p := listPage{raw: o.raw}
	if err := json.Unmarshal(o.raw, &p); err != nil {
		t.Fatalf("%v: %s", err, o.raw)
	}
	return p
}

// listInboxesErr calls list_inboxes with after and returns the tool error, which
// must be there.
func listInboxesErr(t *testing.T, cs *mcp.ClientSession, after string) string {
	t.Helper()
	o := callTool(t, cs, "list_inboxes", map[string]any{"after": after})
	if o.err == "" {
		t.Fatalf("list_inboxes(after=%q) succeeded: %s", after, o.raw)
	}
	return o.err
}

// pageThrough follows next_after from the first page to the last, and returns
// the size of each page and everything listed, in order.
func pageThrough(t *testing.T, cs *mcp.ClientSession) (sizes []int, all []string) {
	t.Helper()
	after := ""
	for range 20 {
		p := listInboxesPage(t, cs, after)
		sizes = append(sizes, len(p.Inboxes))
		all = append(all, p.slugs()...)
		if p.Truncated != (p.NextAfter != "") {
			t.Errorf("page %d: truncated=%v but next_after=%q", len(sizes), p.Truncated, p.NextAfter)
		}
		if p.NextAfter == "" {
			return sizes, all
		}
		after = p.NextAfter
	}
	t.Fatal("list_inboxes kept returning next_after")
	return nil, nil
}

// agentToken makes an agent token for these inboxes, in this order.
func (ae *agentEnv) agentToken(ins ...*inbox.Inbox) string {
	ae.t.Helper()
	tok, hash, err := token.Generate()
	if err != nil {
		ae.t.Fatal(err)
	}
	ids := make([]inbox.ID, 0, len(ins))
	for _, in := range ins {
		ids = append(ids, in.ID)
	}
	if err := ae.store.CreateToken(ae.t.Context(), &inbox.Token{Name: "paging", Hash: hash, InboxIDs: ids}); err != nil {
		ae.t.Fatal(err)
	}
	return tok
}

// slugPrincipal reads the inboxes whose slugs it holds, and is not limited to a
// list, so list_inboxes searches for them.
type slugPrincipal struct {
	mu    *sync.Mutex
	slugs map[string]bool
}

func (p slugPrincipal) CanReadInbox(in *inbox.Inbox) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return in != nil && p.slugs[in.Slug]
}
func (slugPrincipal) CanAdmin() bool { return false }

// slugAuth authenticates the slugPrincipals and scopedListPrincipals it holds by
// bearer token, and defers to inner for every other token.
type slugAuth struct {
	inner  inbox.Authenticator
	mu     sync.Mutex
	byTok  map[string]inbox.Principal
	scoped map[string]scopedListPrincipal
}

func newSlugAuth() *slugAuth {
	return &slugAuth{byTok: map[string]inbox.Principal{}, scoped: map[string]scopedListPrincipal{}}
}

func (a *slugAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	a.mu.Lock()
	p, ok := a.byTok[tok]
	sp, sok := a.scoped[tok]
	a.mu.Unlock()
	switch {
	case ok:
		return p, nil
	case sok:
		return sp, nil
	}
	return a.inner.Authenticate(r)
}

// reads registers an unscoped principal for tok that reads these slugs.
func (a *slugAuth) reads(tok string, slugs ...string) {
	p := slugPrincipal{mu: &a.mu, slugs: map[string]bool{}}
	for _, s := range slugs {
		p.slugs[s] = true
	}
	a.mu.Lock()
	a.byTok[tok] = p
	a.mu.Unlock()
}

// liveScope is a principal limited to a list of inboxes, as an agent token is,
// but to any number of them. Like an agent token's, its list loses an inbox
// when the inbox is deleted (drop).
type liveScope struct {
	mu  sync.Mutex
	ids []inbox.ID
}

func (p *liveScope) CanReadInbox(in *inbox.Inbox) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return in != nil && slices.Contains(p.ids, in.ID)
}
func (*liveScope) CanAdmin() bool { return false }
func (p *liveScope) InboxIDs(context.Context) ([]inbox.ID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ids), nil
}

func (p *liveScope) drop(id inbox.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = slices.DeleteFunc(p.ids, func(x inbox.ID) bool { return x == id })
}

// scopedTo registers a principal for tok limited to these inboxes, in this order.
func (a *slugAuth) scopedTo(tok string, ins ...*inbox.Inbox) *liveScope {
	p := &liveScope{}
	for _, in := range ins {
		p.ids = append(p.ids, in.ID)
	}
	a.mu.Lock()
	a.byTok[tok] = p
	a.mu.Unlock()
	return p
}

func slugRange(prefix string, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("%s-%03d", prefix, i))
	}
	return out
}

// A principal limited to 250 inboxes reads them all, 100 at a time, in the
// order of its list, with no repeats or gaps.
func TestListInboxesPagesAScopedPrincipal(t *testing.T) {
	sa := newSlugAuth()
	ae, base, _ := mcpEnv(t, func(o *Options) { sa.inner = o.Authenticator; o.Authenticator = sa })
	var ins []*inbox.Inbox
	var want []string
	// Created in one order, given to the token in another, so the order the
	// token lists them in is visibly its own.
	for i := range 250 {
		ins = append(ins, ae.newInbox(fmt.Sprintf("scoped-%03d", i), "trusted", ""))
	}
	slices.Reverse(ins)
	for _, in := range ins {
		want = append(want, in.Slug)
	}
	sa.scopedTo("scoped-250", ins...)
	cs := connectMCP(t, base, "scoped-250")
	sizes, all := pageThrough(t, cs)
	if !slices.Equal(sizes, []int{100, 100, 50}) || !slices.Equal(all, want) {
		t.Errorf("pages of %v; listed %d inboxes, want the token's 250 in its order", sizes, len(all))
	}

	// Exactly a page is one page: not truncated, nothing to continue from. (An
	// agent token is limited to 100 inboxes, so this is all it ever sees.)
	cs = connectMCP(t, base, ae.agentToken(ins[:mcpMaxInboxes]...))
	p := listInboxesPage(t, cs, "")
	if len(p.Inboxes) != mcpMaxInboxes || p.Truncated || p.NextAfter != "" {
		t.Errorf("exactly %d inboxes: got %d, truncated=%v, next_after=%q", mcpMaxInboxes, len(p.Inboxes), p.Truncated, p.NextAfter)
	}
}

// A principal that can read any inbox pages through all of them in ID order.
func TestListInboxesPagesAnUnscopedPrincipal(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	// agent-a and agent-b are there already: 248 more make 250.
	want := []string{"agent-a", "agent-b"}
	for _, s := range slugRange("all", 0, 248) {
		ae.newInbox(s, "trusted", "")
		want = append(want, s)
	}
	cs := connectMCP(t, base, testAdminToken)
	sizes, all := pageThrough(t, cs)
	if !slices.Equal(sizes, []int{100, 100, 50}) || !slices.Equal(all, want) {
		t.Errorf("pages of %v; listed %d inboxes (%v...), want all 250 in order", sizes, len(all), all[:min(3, len(all))])
	}
}

// Exactly a page of readable inboxes, with others around them, is one page.
func TestListInboxesExactlyAPageIsNotTruncated(t *testing.T) {
	sa := newSlugAuth()
	ae, base, _ := mcpEnv(t, func(o *Options) { sa.inner = o.Authenticator; o.Authenticator = sa })
	for _, s := range slugRange("x", 0, 150) {
		ae.newInbox(s, "trusted", "")
	}
	sa.reads("hundred", slugRange("x", 20, 120)...)
	p := listInboxesPage(t, connectMCP(t, base, "hundred"), "")
	if !slices.Equal(p.slugs(), slugRange("x", 20, 120)) || p.Truncated || p.NextAfter != "" {
		t.Errorf("got %d inboxes, truncated=%v, next_after=%q", len(p.Inboxes), p.Truncated, p.NextAfter)
	}
	// What a client that never pages sees is what it saw before paging existed.
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(p.raw, &keys)
	if len(keys) != 1 || keys["inboxes"] == nil {
		t.Errorf("an untruncated list has fields besides inboxes: %s", p.raw[:min(200, len(p.raw))])
	}
}

// Inboxes a principal cannot read, in the middle of what it can, are passed over
// on every page, in both kinds of list.
func TestListInboxesPagingSkipsUnreadableInboxes(t *testing.T) {
	sa := newSlugAuth()
	ae, base, _ := mcpEnv(t, func(o *Options) { sa.inner = o.Authenticator; o.Authenticator = sa })
	var readable []string
	sp := scopedListPrincipal{readable: map[inbox.ID]bool{}}
	for i := range 300 {
		in := ae.newInbox(fmt.Sprintf("mix-%03d", i), "trusted", "")
		sp.ids = append(sp.ids, in.ID)
		if i%6 != 5 { // every sixth cannot be read: 250 can
			readable = append(readable, in.Slug)
			sp.readable[in.ID] = true
		}
	}
	gone := ae.newInbox("mix-gone", "trusted", "")
	if err := ae.store.DeleteInbox(t.Context(), gone.ID); err != nil {
		t.Fatal(err)
	}
	// The scoped list also names an inbox that no longer exists, in the middle.
	sp.ids = slices.Insert(sp.ids, 150, gone.ID)
	sa.scoped["scoped-mix"] = sp
	sa.reads("unscoped-mix", readable...)

	for _, tok := range []string{"scoped-mix", "unscoped-mix"} {
		t.Run(tok, func(t *testing.T) {
			sizes, all := pageThrough(t, connectMCP(t, base, tok))
			if !slices.Equal(sizes, []int{100, 100, 50}) || !slices.Equal(all, readable) {
				t.Errorf("pages of %v; listed %d inboxes, want the 250 readable ones in order", sizes, len(all))
			}
		})
	}
}

// An inbox deleted between pages does not make the next page skip or repeat any
// other, whether it was on the page already read or on the next.
func TestListInboxesPagingSurvivesDeletedInboxes(t *testing.T) {
	for _, scoped := range []bool{true, false} {
		name := map[bool]string{true: "scoped", false: "unscoped"}[scoped]
		t.Run(name, func(t *testing.T) {
			sa := newSlugAuth()
			ae, base, _ := mcpEnv(t, func(o *Options) { sa.inner = o.Authenticator; o.Authenticator = sa })
			var ins []*inbox.Inbox
			for _, s := range slugRange("churn", 0, 248) {
				ins = append(ins, ae.newInbox(s, "trusted", ""))
			}
			list, tok := append([]*inbox.Inbox{ae.a, ae.b}, ins...), testAdminToken
			scope := sa.scopedTo("churn", ins...)
			if scoped {
				list, tok = ins, "churn"
			}
			del := func(in *inbox.Inbox) {
				t.Helper()
				if err := ae.store.DeleteInbox(t.Context(), in.ID); err != nil {
					t.Fatal(err)
				}
				scope.drop(in.ID)
			}
			slugsOf := func(ins []*inbox.Inbox) []string {
				var out []string
				for _, in := range ins {
					out = append(out, in.Slug)
				}
				return out
			}
			cs := connectMCP(t, base, tok)
			p1 := listInboxesPage(t, cs, "")
			if !slices.Equal(p1.slugs(), slugsOf(list[:100])) || p1.NextAfter == "" {
				t.Fatalf("first page: %d inboxes, next_after=%q", len(p1.Inboxes), p1.NextAfter)
			}
			// One from the page already read, and one from the next.
			read, next := list[50], list[150]
			del(read)
			del(next)
			var want []*inbox.Inbox
			for _, in := range list[100:] {
				if in != next {
					want = append(want, in)
				}
			}
			p2 := listInboxesPage(t, cs, p1.NextAfter)
			if !slices.Equal(p2.slugs(), slugsOf(want[:100])) || !p2.Truncated || p2.NextAfter == "" {
				got := p2.slugs()
				t.Fatalf("second page after deletes: %d inboxes from %v, truncated=%v; want 100 from %s",
					len(got), got[:min(1, len(got))], p2.Truncated, want[0].Slug)
			}
			p3 := listInboxesPage(t, cs, p2.NextAfter)
			if !slices.Equal(p3.slugs(), slugsOf(want[100:])) || p3.Truncated || p3.NextAfter != "" {
				t.Errorf("last page: %d inboxes, truncated=%v, next_after=%q; want %d", len(p3.Inboxes), p3.Truncated, p3.NextAfter, len(want)-100)
			}

			// The inbox a cursor continues from is itself deleted: the call says
			// so instead of guessing where to continue.
			del(want[99])
			if msg := listInboxesErr(t, cs, p2.NextAfter); !strings.Contains(msg, "after") || !strings.Contains(msg, "without after") {
				t.Errorf("a cursor whose inbox was deleted: %q", msg)
			}
			// Nor does an inbox made again under the same slug stand in for it:
			// it is a new inbox, later in the list.
			again := ae.newInbox(want[99].Slug, "trusted", "")
			scope.mu.Lock()
			scope.ids = slices.Insert(scope.ids, 0, again.ID)
			scope.mu.Unlock()
			if msg := listInboxesErr(t, cs, p2.NextAfter); !strings.Contains(msg, "without after") {
				t.Errorf("a cursor whose inbox was made again: %q", msg)
			}
		})
	}
}

// A cursor that is not one list_inboxes returned is refused, and says which
// argument is wrong.
func TestListInboxesRefusesAMalformedCursor(t *testing.T) {
	ae, base, _ := mcpEnv(t)
	for _, s := range slugRange("bad", 0, 120) {
		ae.newInbox(s, "trusted", "")
	}
	cs := connectMCP(t, base, testAdminToken)
	good := listInboxesPage(t, cs, "").NextAfter
	if good == "" {
		t.Fatal("no cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(good)
	if err != nil {
		t.Fatalf("the cursor is not base64url: %q", good)
	}
	flipped := slices.Clone(raw)
	flipped[len(flipped)/2] ^= 1
	for name, after := range map[string]string{
		"not base64":      "not a cursor!",
		"a number":        "100",
		"junk":            base64.RawURLEncoding.EncodeToString([]byte("junk junk junk junk")),
		"truncated":       good[:len(good)-3],
		"one bit changed": base64.RawURLEncoding.EncodeToString(flipped),
		"padded":          good + "==",
	} {
		t.Run(name, func(t *testing.T) {
			msg := listInboxesErr(t, cs, after)
			if !strings.HasPrefix(msg, "invalid after") {
				t.Errorf("error %q does not say the argument is invalid", msg)
			}
		})
	}
	// The document limits its length, and the SDK refuses a longer one before
	// the tool sees it.
	if msg := listInboxesErr(t, cs, strings.Repeat("A", 4096)); !strings.Contains(msg, "after") {
		t.Errorf("a cursor over the length limit: %.100s", msg)
	}
}

// A cursor carries nothing a caller could not see already, and taking one from
// another caller's list shows no inbox the caller cannot read, and does not say
// whether the inbox it continues from exists.
func TestListInboxesCursorFromAnotherCaller(t *testing.T) {
	sa := newSlugAuth()
	ae, base, _ := mcpEnv(t, func(o *Options) { sa.inner = o.Authenticator; o.Authenticator = sa })
	var ins []*inbox.Inbox
	for _, s := range slugRange("cross", 0, 300) {
		ins = append(ins, ae.newInbox(s, "trusted", ""))
	}
	sa.reads("first", slugRange("cross", 0, 150)...)                                             // reads 0-149
	sa.reads("second", slugRange("cross", 150, 300)...)                                          // reads 150-299
	sa.reads("overlap", append(slugRange("cross", 90, 110), slugRange("cross", 200, 230)...)...) // some of each
	scopedFirst := "scoped-first"
	sa.scopedTo(scopedFirst, ins[:150]...)

	cs := connectMCP(t, base, "first")
	cursor := listInboxesPage(t, cs, "").NextAfter
	if cursor == "" {
		t.Fatal("no cursor")
	}
	// It does not carry the inbox's ID, which would say how many inboxes the
	// server has made, in any of the usual encodings.
	raw, _ := base64.RawURLEncoding.DecodeString(cursor)
	var be8, le8 [8]byte
	binary.BigEndian.PutUint64(be8[:], uint64(ins[99].ID))    //nolint:gosec // an ID is positive
	binary.LittleEndian.PutUint64(le8[:], uint64(ins[99].ID)) //nolint:gosec // an ID is positive
	for _, leak := range [][]byte{be8[:], le8[:], be8[4:], le8[:4]} {
		if bytes.Contains(raw, leak) {
			t.Errorf("the cursor %q carries the inbox ID %d (as %x)", cursor, ins[99].ID, leak)
		}
	}
	if len(cursor) > 200 {
		t.Errorf("the cursor is %d bytes", len(cursor))
	}

	// The inbox the cursor continues from cannot be read by "second": the call
	// fails just as it does for an inbox that does not exist.
	second := connectMCP(t, base, "second")
	readGone := listInboxesErr(t, second, cursor)
	if err := ae.store.DeleteInbox(t.Context(), ins[99].ID); err != nil {
		t.Fatal(err)
	}
	gone := listInboxesErr(t, connectMCP(t, base, "first"), cursor)
	if readGone != gone {
		t.Errorf("a cursor at an inbox the caller cannot read (%q) differs from one at an inbox that is gone (%q)", readGone, gone)
	}

	// A caller that can read the inbox the cursor names continues from it, and
	// sees only its own.
	sa.reads("overlap2", append(slugRange("cross", 98, 99), append(slugRange("cross", 100, 110), slugRange("cross", 200, 230)...)...)...)
	c2 := listInboxesPage(t, connectMCP(t, base, "first"), "").NextAfter
	p := listInboxesPage(t, connectMCP(t, base, "overlap2"), c2)
	want := append(slugRange("cross", 101, 110), slugRange("cross", 200, 230)...)
	if !slices.Equal(p.slugs(), want) || p.Truncated || p.NextAfter != "" {
		t.Errorf("overlap2 continuing from first's cursor: %v", p.slugs())
	}

	// A scoped token's cursor given to an unscoped principal, and the other way
	// round, is refused rather than read in the other list's terms.
	scoped := listInboxesPage(t, connectMCP(t, base, scopedFirst), "").NextAfter
	if scoped == "" {
		t.Fatal("no scoped cursor")
	}
	if msg := listInboxesErr(t, connectMCP(t, base, "overlap"), scoped); !strings.Contains(msg, "after") {
		t.Errorf("scoped cursor to an unscoped principal: %q", msg)
	}
	if msg := listInboxesErr(t, connectMCP(t, base, scopedFirst), c2); !strings.Contains(msg, "after") {
		t.Errorf("unscoped cursor to a scoped token: %q", msg)
	}
}

// Running out of scan budget is still not reported when paging: a page that
// fills says there is more, a page that runs out of budget first says nothing.
func TestListInboxesPagingDoesNotRevealTheScanBudget(t *testing.T) {
	old := mcpListScan
	mcpListScan = 150
	t.Cleanup(func() { mcpListScan = old })
	sa := newSlugAuth()
	ae, base, _ := mcpEnv(t, func(o *Options) { sa.inner = o.Authenticator; o.Authenticator = sa })
	for _, s := range slugRange("scan", 0, 260) {
		ae.newInbox(s, "trusted", "")
	}
	sa.reads("dense", append(slugRange("scan", 0, 120), slugRange("scan", 200, 260)...)...)
	cs := connectMCP(t, base, "dense")
	// agent-a and agent-b come first and cannot be read; the page fills within
	// the budget.
	p1 := listInboxesPage(t, cs, "")
	if !slices.Equal(p1.slugs(), slugRange("scan", 0, 100)) || !p1.Truncated || p1.NextAfter == "" {
		t.Fatalf("first page: %d inboxes, truncated=%v, next_after=%q", len(p1.Inboxes), p1.Truncated, p1.NextAfter)
	}
	// From scan-100 the budget covers scan-100 to scan-249: 70 readable, then it
	// runs out with scan-250 to scan-259 unread, and says nothing about it.
	p2 := listInboxesPage(t, cs, p1.NextAfter)
	if !slices.Equal(p2.slugs(), append(slugRange("scan", 100, 120), slugRange("scan", 200, 250)...)) {
		t.Errorf("second page: %v", p2.slugs())
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(p2.raw, &keys)
	if p2.Truncated || p2.NextAfter != "" || len(keys) != 1 {
		t.Errorf("the scan budget running out was reported: truncated=%v, next_after=%q, fields %d", p2.Truncated, p2.NextAfter, len(keys))
	}
}
