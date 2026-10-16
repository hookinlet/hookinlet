// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import (
	"context"
	"unicode/utf8"
)

// MaxTenantLen bounds a tenant key, in bytes.
const MaxTenantLen = 128

// ValidTenant reports whether s can be a tenant key: at most MaxTenantLen bytes
// of valid UTF-8. The empty string is the default tenant, the only one an
// installation that does not use tenants has.
func ValidTenant(s string) bool { return len(s) <= MaxTenantLen && utf8.ValidString(s) }

type tenantKey struct{}

// WithTenant returns a context that scopes Store calls to one tenant: see the
// Store's "Tenants" section. The empty string scopes to the default tenant, which
// is not the same as not scoping at all.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenant)
}

// TenantFrom returns the tenant ctx is scoped to. scoped is false when ctx
// carries none, which is how the server calls the Store for a delivery or a
// background job, and how the command line does.
func TenantFrom(ctx context.Context) (tenant string, scoped bool) {
	t, ok := ctx.Value(tenantKey{}).(string)
	return t, ok
}

// TenantPrincipal is an optional capability of a Principal that belongs to one
// tenant. After authenticating a request the server scopes the request's context
// to the principal's tenant (WithTenant), before any other use of the Store, so
// every slug the caller names is looked up among that tenant's inboxes and
// nothing outside it can be listed, read or changed. A Principal that does not
// implement it leaves the context unscoped.
//
// Tenant must be the same for the life of the principal. An embedder whose callers
// belong to a tenant must implement this for every such Principal: one that does
// not is unscoped, and an unscoped caller names inboxes in the default tenant.
type TenantPrincipal interface {
	Principal
	// Tenant returns the principal's tenant key; see Inbox.Tenant.
	Tenant() string
}
