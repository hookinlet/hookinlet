// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/verify"
)

// These interfaces are implemented outside this module, so a method added to one
// breaks every implementation. A new capability is a new interface that an
// implementation may also satisfy, found with a type assertion (see Admitter, Backupper and
// ScopedPrincipal and docs/stability.md). This test fails when a method set changes,
// so that the change is a decision and not an accident; before 1.0 a decision to
// change one is allowed, and goes in CHANGELOG.md, and this list is updated with it.
func TestTheCoreInterfacesOnlyChangeOnPurpose(t *testing.T) {
	// Each method with its signature: a changed parameter or result breaks every
	// implementation as much as a new method does.
	want := map[string][]string{
		"Store": {
			"Append(context.Context, inbox.ID, *inbox.NewMessage) (*inbox.Message, bool, error)",
			"Close() error",
			"CreateInbox(context.Context, *inbox.Inbox) error",
			"CreateToken(context.Context, *inbox.Token) error",
			"DeleteInbox(context.Context, inbox.ID) error",
			"DeleteToken(context.Context, inbox.TokenID) error",
			"Get(context.Context, inbox.ID, int64) (*inbox.Message, error)",
			"GetInbox(context.Context, inbox.ID) (*inbox.Inbox, error)",
			"GetInboxBySlug(context.Context, string) (*inbox.Inbox, error)",
			"GetTokenByHash(context.Context, []uint8) (*inbox.Token, error)",
			"LatestSeq(context.Context, inbox.ID) (int64, error)",
			"ListInboxes(context.Context, inbox.ListParams) ([]*inbox.Inbox, error)",
			"ListTokens(context.Context, inbox.TokenListParams) ([]*inbox.Token, error)",
			"Ping(context.Context) error",
			"Prune(context.Context, time.Time) (int64, error)",
			"Query(context.Context, inbox.ID, inbox.Query) (*inbox.Page, error)",
			"ResolveIngestKey(context.Context, string) (*inbox.Inbox, error)",
			"Stats(context.Context, inbox.ID) (*inbox.InboxStats, error)",
			"UpdateInbox(context.Context, *inbox.Inbox) error",
		},
		"Hooks": {
			"AfterIngest(context.Context, *inbox.Inbox, *inbox.Message, bool)",
			"BeforeIngest(context.Context, *inbox.IngestRequest) error",
		},
		"SecretBox": {
			"Open([]uint8, []uint8) ([]uint8, error)",
			"Seal([]uint8, []uint8) ([]uint8, error)",
		},
		"Authenticator": {"Authenticate(*http.Request) (inbox.Principal, error)"},
		"Principal":     {"CanAdmin() bool", "CanReadInbox(*inbox.Inbox) bool"},
		"Notifier": {
			"Publish(context.Context, inbox.ID, int64)",
			"Subscribe(context.Context, inbox.ID) (<-chan struct {}, func())",
		},
		// The optional interfaces are part of the surface too.
		"Admitter":          {"Admit(context.Context, inbox.Principal, *inbox.Inbox, inbox.AccessKind) (func(), error)"},
		"Backupper":         {"Backup(context.Context) (io.ReadCloser, int64, error)"},
		"IngestObserver":    {"ObserveIngest(context.Context, *inbox.IngestEvent)"},
		"UnverifiedLimiter": {"MaxUnverified(context.Context, *inbox.Inbox) int"},
		"UnverifiedCounter": {"CountUnverified(context.Context, inbox.ID, int64) (int64, error)"},
		"ScopedPrincipal":   {"CanAdmin() bool", "CanReadInbox(*inbox.Inbox) bool", "InboxIDs(context.Context) ([]inbox.ID, error)"},
		"TenantPrincipal":   {"CanAdmin() bool", "CanReadInbox(*inbox.Inbox) bool", "Tenant() string"},
		"Verifier":          {"Verify(*http.Request, []uint8, []uint8, time.Time) (verify.Result, error)"},
	}
	got := map[string]reflect.Type{
		"Store":             reflect.TypeFor[inbox.Store](),
		"Hooks":             reflect.TypeFor[inbox.Hooks](),
		"SecretBox":         reflect.TypeFor[inbox.SecretBox](),
		"Authenticator":     reflect.TypeFor[inbox.Authenticator](),
		"Principal":         reflect.TypeFor[inbox.Principal](),
		"Notifier":          reflect.TypeFor[inbox.Notifier](),
		"Admitter":          reflect.TypeFor[inbox.Admitter](),
		"TenantPrincipal":   reflect.TypeFor[inbox.TenantPrincipal](),
		"Backupper":         reflect.TypeFor[inbox.Backupper](),
		"IngestObserver":    reflect.TypeFor[inbox.IngestObserver](),
		"UnverifiedLimiter": reflect.TypeFor[inbox.UnverifiedLimiter](),
		"UnverifiedCounter": reflect.TypeFor[inbox.UnverifiedCounter](),
		"ScopedPrincipal":   reflect.TypeFor[inbox.ScopedPrincipal](),
		"Verifier":          reflect.TypeFor[verify.Verifier](),
	}
	for name, typ := range got {
		var methods []string
		for m := range typ.Methods() {
			sig := strings.TrimPrefix(m.Type.String(), "func")
			methods = append(methods, m.Name+sig)
		}
		if strings.Join(methods, "\n") != strings.Join(want[name], "\n") {
			t.Errorf("%s changed:\n got:\n  %s\n want:\n  %s\nAdd a capability as a new optional interface instead (docs/stability.md); if this change is deliberate, say so in CHANGELOG.md and update this list.",
				name, strings.Join(methods, "\n  "), strings.Join(want[name], "\n  "))
		}
	}
}
