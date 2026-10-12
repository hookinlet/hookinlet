// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// These interfaces are implemented outside this module, so a method added to one
// breaks every implementation. A new capability is a new interface that an
// implementation may also satisfy, found with a type assertion (see Backupper and
// ScopedPrincipal and docs/stability.md). This test fails when a method set changes,
// so that the change is a decision and not an accident; before 1.0 a decision to
// change one is allowed, and goes in CHANGELOG.md, and this list is updated with it.
func TestTheCoreInterfacesOnlyChangeOnPurpose(t *testing.T) {
	want := map[string][]string{
		"Store": {
			"Append", "Close", "CreateInbox", "CreateToken", "DeleteInbox", "DeleteToken", "Get", "GetInbox", "GetInboxBySlug",
			"GetTokenByHash", "LatestSeq", "ListInboxes", "ListTokens", "Ping", "Prune", "Query", "ResolveIngestKey",
			"Stats", "UpdateInbox",
		},
		"Hooks":         {"AfterIngest", "BeforeIngest"},
		"SecretBox":     {"Open", "Seal"},
		"Authenticator": {"Authenticate"},
		"Principal":     {"CanAdmin", "CanReadInbox"},
		"Notifier":      {"Publish", "Subscribe"},
	}
	got := map[string]reflect.Type{
		"Store":         reflect.TypeFor[inbox.Store](),
		"Hooks":         reflect.TypeFor[inbox.Hooks](),
		"SecretBox":     reflect.TypeFor[inbox.SecretBox](),
		"Authenticator": reflect.TypeFor[inbox.Authenticator](),
		"Principal":     reflect.TypeFor[inbox.Principal](),
		"Notifier":      reflect.TypeFor[inbox.Notifier](),
	}
	for name, typ := range got {
		var methods []string
		for m := range typ.Methods() {
			methods = append(methods, m.Name)
		}
		if strings.Join(methods, ",") != strings.Join(want[name], ",") {
			t.Errorf("inbox.%s changed:\n got  %s\n want %s\nAdd a capability as a new optional interface instead (docs/stability.md); if this change is deliberate, say so in CHANGELOG.md and update this list.",
				name, fmt.Sprint(methods), fmt.Sprint(want[name]))
		}
	}
}
