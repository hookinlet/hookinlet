// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
	"github.com/hookinlet/hookinlet/pkg/server/servertest"
	"github.com/hookinlet/hookinlet/pkg/store/sqlite"
)

// The server passes its own conformance suite for embedders, over the SQLite store and a
// small authenticator whose principals belong to tenants. This is an external test
// package because servertest imports the server.

type tenantPrincipal struct {
	tenant string
	admin  bool
}

func (p tenantPrincipal) CanReadInbox(in *inbox.Inbox) bool {
	return in != nil && in.Tenant == p.tenant
}
func (p tenantPrincipal) CanAdmin() bool { return p.admin }
func (p tenantPrincipal) Tenant() string { return p.tenant }

// noTenant says yes to every inbox and is in no tenant; with admin set it is the operator.
type noTenant struct{ admin bool }

func (p noTenant) CanReadInbox(in *inbox.Inbox) bool { return in != nil }
func (p noTenant) CanAdmin() bool                    { return p.admin }

type suiteAuth struct{}

func (suiteAuth) Authenticate(r *http.Request) (inbox.Principal, error) {
	switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
	case "reader-a":
		return tenantPrincipal{tenant: "org-a"}, nil
	case "reader-b":
		return tenantPrincipal{tenant: "org-b"}, nil
	case "admin-a":
		return tenantPrincipal{tenant: "org-a", admin: true}, nil
	case "operator":
		return noTenant{admin: true}, nil
	case "careless":
		return noTenant{}, nil
	}
	return nil, inbox.ErrUnauthenticated
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

func TestServerSuite(t *testing.T) {
	servertest.ServerSuite(t, servertest.Harness{
		NewStore: func(t *testing.T, now func() time.Time) inbox.Store {
			s, err := sqlite.Open(t.Context(), t.TempDir(), sqlite.WithClock(now))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		NewAuthenticator: func(*testing.T, inbox.Store) (inbox.Authenticator, servertest.Callers) {
			return suiteAuth{}, servertest.Callers{
				ReaderA:  bearer("reader-a"),
				ReaderB:  bearer("reader-b"),
				AdminA:   bearer("admin-a"),
				Operator: bearer("operator"),
				NoTenant: bearer("careless"),
			}
		},
	})
}
