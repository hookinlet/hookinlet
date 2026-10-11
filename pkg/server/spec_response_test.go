// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"testing"
)

// Every response the server gives in any test that goes through env.do or the
// admin client is checked against docs/openapi.json: the operation must exist,
// the status must be one it lists, the required headers must be present, and the
// body must match its schema (response objects closed). A test that provokes an
// answer the document does not describe fails there, whatever it set out to test.

var (
	routesOnce sync.Once
	routeSpec  *spec
	routes     []route
)

type route struct {
	re     *regexp.Regexp
	method string
	opID   string
	weight int // literal characters: the most specific template wins
}

func loadRoutes(t *testing.T) {
	routesOnce.Do(func() {
		routeSpec = loadSpec(t)
		for _, op := range routeSpec.doc.Operations() {
			pat := regexp.QuoteMeta(op.Path)
			pat = regexp.MustCompile(`\\\{rest\\\}`).ReplaceAllString(pat, `.+`)
			pat = regexp.MustCompile(`\\\{[a-z]+\\\}`).ReplaceAllString(pat, `[^/]+`)
			routes = append(routes, route{regexp.MustCompile("^" + pat + "$"), op.Method, op.ID, len(regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(op.Path, ""))})
		}
	})
}

// checkAgainstSpec checks one response. A request to a route the document does
// not describe (the catch-all 404, for instance) is not checked.
func checkAgainstSpec(t *testing.T, method, target string, status int, hdr http.Header, body []byte) {
	t.Helper()
	loadRoutes(t)
	u, err := url.Parse(target)
	if err != nil {
		return
	}
	var best *route
	for i := range routes {
		r := &routes[i]
		if r.method == method && r.re.MatchString(u.Path) && (best == nil || r.weight > best.weight) {
			best = r
		}
	}
	if best == nil {
		// A method the path does not take is answered 405 for the operations that
		// path does have.
		if status == http.StatusMethodNotAllowed {
			for i := range routes {
				// The operation that is not a HEAD one: its 405 has the body.
				if routes[i].re.MatchString(u.Path) && routes[i].method != http.MethodHead && (best == nil || routes[i].weight > best.weight) {
					best = &routes[i]
				}
			}
		}
		if best == nil {
			return
		}
	}
	c := &contract{t: t, sp: routeSpec, seen: map[string]map[string]bool{}}
	c.record(best.opID, status, hdr, body, best.method == http.MethodHead, false)
}
