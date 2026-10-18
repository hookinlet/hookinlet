// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

type capHooks struct {
	inbox.NopHooks
	cap int
}

func (h capHooks) MaxUnverified(context.Context, *inbox.Inbox) int { return h.cap }

// The warning for an inbox at its cap names the cap that applied, not the server's.
func TestUnverifiedLimitLogNamesTheCapThatApplied(t *testing.T) {
	for _, c := range []struct {
		name    string
		server  int
		limiter int
		want    string
	}{
		{"server cap", 2, 0, `"max_unverified":2`},
		{"limiter cap", 50, 1, `"max_unverified":1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(o *Options) {
				o.MaxUnverified = c.server
				o.Hooks = capHooks{cap: c.limiter}
			})
			e.newInbox("capped", "none", "")
			for range 5 {
				if rec := e.do(req{path: "/in/capped", body: "x"}); rec.Code == http.StatusTooManyRequests {
					if !strings.Contains(e.logs.String(), c.want) {
						t.Fatalf("log does not say %s:\n%s", c.want, e.logs.String())
					}
					return
				}
			}
			t.Fatal("the cap was never reached")
		})
	}
}
