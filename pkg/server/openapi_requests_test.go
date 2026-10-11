// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The document says what a request may contain. These tests generate requests
// from those schemas and hold the server to them in both directions: a request
// that breaks a schema must be refused, and one at the edge of what the schema
// allows must be accepted. A schema looser than the server (it promises what the
// server refuses) or stricter (it forbids what the server takes) fails here.

// invalidValues returns values that break one constraint of prop's schema.
func invalidValues(prop map[string]any) map[string]any {
	out := map[string]any{}
	t, _ := prop["type"].(string)
	switch t {
	case "string":
		out["a number instead of a string"] = 12345
	case "integer", "number":
		out["a string instead of a number"] = "not-a-number"
	case "boolean":
		out["a string instead of a boolean"] = "yes"
	case "object":
		out["a string instead of an object"] = "x"
	case "array":
		out["a string instead of an array"] = "x"
	}
	if e, ok := prop["enum"].([]any); ok && len(e) > 0 {
		out["a value outside the enum"] = "zzz-not-allowed"
	}
	if m, ok := prop["minimum"].(float64); ok {
		out["below the minimum"] = m - 1
	}
	if m, ok := prop["maximum"].(float64); ok {
		out["above the maximum"] = m + 1
	}
	if _, ok := prop["pattern"].(string); ok && t == "string" {
		out["not matching the pattern"] = "!!bad value!!"
	}
	if m, ok := prop["minLength"].(float64); ok && m >= 1 {
		out["shorter than minLength"] = strings.Repeat("a", int(m)-1)
	}
	if m, ok := prop["maxLength"].(float64); ok {
		out["longer than maxLength"] = strings.Repeat("a", int(m)+1)
	}
	return out
}

// edgeValues returns valid values at the edges of prop's schema, keyed by a
// description. The properties whose meaning the server checks further than the
// schema can say (which verifier exists, what a configuration contains) are left
// to the ordinary tests.
func edgeValues(name string, prop map[string]any) map[string]any {
	out := map[string]any{}
	t, _ := prop["type"].(string)
	switch name {
	case "verifier", "verifier_config":
		return out
	case "slug":
		return map[string]any{"shortest": "abc", "longest": strings.Repeat("a", 64)}
	case "event_type_pointer":
		return map[string]any{"empty": "", "a pointer": "/type", "escapes": "/a~0b~1c"}
	}
	if e, ok := prop["enum"].([]any); ok {
		for _, v := range e {
			out[fmt.Sprintf("enum %v", v)] = v
		}
		return out
	}
	switch t {
	case "boolean":
		out["true"], out["false"] = true, false
	case "integer":
		if m, ok := prop["minimum"].(float64); ok {
			out["the minimum"] = m
		}
		if m, ok := prop["maximum"].(float64); ok {
			out["the maximum"] = m
		}
	case "string":
		if m, ok := prop["maxLength"].(float64); ok {
			out["the longest"] = strings.Repeat("a", int(m))
		}
		if m, ok := prop["minLength"].(float64); ok {
			out["the shortest"] = strings.Repeat("a", int(m))
		}
	}
	return out
}

func TestOpenAPIRequestBodiesAreHeldToTheirSchemas(t *testing.T) {
	sp := loadSpec(t)
	a := newAdminEnv(t)
	cl := a.client(t)
	bad := get(sp.root, "components", "schemas", "ErrorBadRequest", "properties", "error", "properties", "code", "enum").([]any)

	type target struct {
		opID, method string
		path         func(n int) string
		base         func(n int) map[string]any
		okStatus     int
	}
	targets := []target{
		{"createInbox", "POST", func(int) string { return "/admin/inboxes" }, func(n int) map[string]any {
			return map[string]any{"slug": fmt.Sprintf("neg-%03d", n), "verifier": "none"}
		}, 201},
		{"updateInbox", "PATCH", func(int) string { return "/admin/inboxes/agent-a" }, func(int) map[string]any { return map[string]any{"name": "ok"} }, 200},
		{"setVerifierSecret", "PUT", func(int) string { return "/admin/inboxes/agent-a/secret" }, func(int) map[string]any { return map[string]any{"secret": "ok"} }, 200},
	}
	n := 0
	for _, tg := range targets {
		o := sp.op(tg.opID)
		schema := sp.deref(get(o.obj, "requestBody", "content", "application/json", "schema")).(map[string]any)
		props := schema["properties"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Errorf("%s: a request schema must say additionalProperties: false, since the server refuses unknown fields", tg.opID)
		}

		send := func(what string, body map[string]any, want int) {
			t.Helper()
			n++
			if slug, ok := body["slug"].(string); ok && tg.opID == "createInbox" && strings.HasPrefix(slug, "neg-") {
				body["slug"] = fmt.Sprintf("neg-%03d", n)
			}
			status, _, data := cl.exchange(tg.method, tg.path(n), body)
			if status != want {
				t.Errorf("%s, %s: status %d, want %d (%.160s)", tg.opID, what, status, want, data)
				return
			}
			if want == 400 {
				var e struct{ Error struct{ Code string } }
				_ = jsonUnmarshal(data, &e)
				found := false
				for _, c := range bad {
					found = found || c == e.Error.Code
				}
				if !found {
					t.Errorf("%s, %s: error code %q is not one the document lists for a 400", tg.opID, what, e.Error.Code)
				}
			}
		}

		// The base request is itself valid, by the document and by the server.
		if errs := sp.check(schema, tg.base(0), tg.opID+" base"); len(errs) > 0 {
			t.Errorf("the base request is not valid by the schema: %v", errs)
		}
		send("the plain request", tg.base(n), tg.okStatus)

		for name, p := range props {
			prop := sp.deref(p).(map[string]any)
			for what, v := range invalidValues(prop) {
				body := tg.base(n)
				body[name] = v
				if errs := sp.check(schema, body, ""); len(errs) == 0 {
					t.Errorf("%s: the schema accepts %s for %q, so this test cannot tell whether the server refuses it", tg.opID, what, name)
				}
				send(fmt.Sprintf("%s: %s", name, what), body, 400)
			}
			for what, v := range edgeValues(name, prop) {
				body := tg.base(n)
				body[name] = v
				if errs := sp.check(schema, body, ""); len(errs) > 0 {
					t.Errorf("%s: the schema rejects %s for %q: %v", tg.opID, what, name, errs)
					continue
				}
				send(fmt.Sprintf("%s: %s", name, what), body, tg.okStatus)
			}
		}
		body := tg.base(n)
		body["zz_unknown_field"] = 1
		send("an unknown field", body, 400)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				body := tg.base(n)
				delete(body, r.(string))
				send("missing required "+r.(string), body, 400)
			}
		}
	}
}

// Query parameters are held to their schemas in the same way.
func TestOpenAPIQueryParametersAreHeldToTheirSchemas(t *testing.T) {
	sp := loadSpec(t)
	a := newAdminEnv(t)
	for i := 0; i < 3; i++ {
		a.post("/in/agent-a", fmt.Sprintf(`{"n":%d}`, i))
	}
	cl := a.client(t)
	bad := get(sp.root, "components", "schemas", "ErrorBadRequest", "properties", "error", "properties", "code", "enum").([]any)

	cases := []struct{ opID, path, tok string }{
		{"listMessages", "/v1/inboxes/agent-a/messages", a.tokenA},
		{"streamMessages", "/v1/inboxes/agent-a/stream", a.tokenA},
		{"listInboxes", "/admin/inboxes", testAdminToken},
	}
	for _, cs := range cases {
		o := sp.op(cs.opID)
		params, _ := o.obj["parameters"].([]any)
		for _, p := range params {
			pm := sp.deref(p).(map[string]any)
			if pm["in"] != "query" {
				continue
			}
			name := pm["name"].(string)
			schema := sp.deref(pm["schema"]).(map[string]any)
			item := schema
			if schema["type"] == "array" {
				item = sp.deref(schema["items"]).(map[string]any)
			}
			var invalid []string
			switch item["type"] {
			case "integer":
				invalid = append(invalid, "abc", "1.5", "-1x")
				if m, ok := item["minimum"].(float64); ok {
					invalid = append(invalid, strconv.Itoa(int(m)-1))
				}
				if m, ok := item["maximum"].(float64); ok {
					invalid = append(invalid, strconv.Itoa(int(m)+1))
				}
			case "boolean":
				invalid = append(invalid, "maybe")
			case "string":
				if e, ok := item["enum"].([]any); ok && len(e) > 0 {
					invalid = append(invalid, "zzz")
				}
				if item["format"] == "date-time" {
					invalid = append(invalid, "yesterday")
				}
				if m, ok := item["maxLength"].(float64); ok {
					invalid = append(invalid, strings.Repeat("a", int(m)+1))
				}
			}
			for _, v := range invalid {
				status, _, data := cl.exchangeWith(cs.tok, "GET", cs.path+"?"+url.Values{name: {v}}.Encode(), nil)
				if status != 400 {
					t.Errorf("%s ?%s=%.20q: status %d, want 400 (%.120s)", cs.opID, name, v, status, data)
					continue
				}
				var e struct{ Error struct{ Code string } }
				_ = jsonUnmarshal(data, &e)
				if !containsAny(bad, e.Error.Code) {
					t.Errorf("%s ?%s: error code %q is not listed for a 400", cs.opID, name, e.Error.Code)
				}
			}
			// Valid values at the edges must be accepted. (A stream would not return.)
			if cs.opID == "streamMessages" {
				continue
			}
			var valid []string
			switch item["type"] {
			case "integer":
				if m, ok := item["minimum"].(float64); ok {
					valid = append(valid, strconv.Itoa(int(m)))
				}
				if m, ok := item["maximum"].(float64); ok {
					valid = append(valid, strconv.Itoa(int(m)))
				}
				valid = append(valid, "1")
			case "boolean":
				valid = append(valid, "true", "false")
			case "string":
				if e, ok := item["enum"].([]any); ok {
					for _, v := range e {
						valid = append(valid, v.(string))
					}
				}
				if item["format"] == "date-time" {
					valid = append(valid, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00.123456+02:00")
				}
			}
			for _, v := range valid {
				if errs := sp.check(schema, queryValue(schema, v), ""); len(errs) > 0 {
					t.Errorf("%s ?%s=%s: the schema rejects its own edge value: %v", cs.opID, name, v, errs)
				}
				status, _, data := cl.exchangeWith(cs.tok, "GET", cs.path+"?"+url.Values{name: {v}}.Encode(), nil)
				if status != 200 {
					t.Errorf("%s ?%s=%s: status %d, want 200 (%.120s)", cs.opID, name, v, status, data)
				}
			}
		}
		// A parameter nobody documented, and a repeated one that is not an array.
		for _, q := range []string{"bogus=1"} {
			if status, _, _ := cl.exchangeWith(cs.tok, "GET", cs.path+"?"+q, nil); status != 400 {
				t.Errorf("%s ?%s: status %d, want 400", cs.opID, q, status)
			}
		}
		for _, p := range params {
			pm := sp.deref(p).(map[string]any)
			if pm["in"] == "query" && sp.deref(pm["schema"]).(map[string]any)["type"] != "array" && pm["name"] != "limit" {
				q := pm["name"].(string) + "=1&" + pm["name"].(string) + "=2"
				if status, _, _ := cl.exchangeWith(cs.tok, "GET", cs.path+"?"+q, nil); status != 400 {
					t.Errorf("%s ?%s: a repeated parameter got %d, want 400", cs.opID, q, status)
				}
				break
			}
		}
	}
}

// queryValue turns a query string value into the JSON value the schema describes.
func queryValue(schema map[string]any, v string) any {
	switch schema["type"] {
	case "integer":
		n, _ := strconv.Atoi(v)
		return float64(n)
	case "boolean":
		return v == "true"
	}
	return v
}

func containsAny(list []any, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// A small client for these tests that does not record against the contract.
type plainClient struct {
	t     *testing.T
	base  string
	admin string
}

func (a *adminEnv) client(t *testing.T) *plainClient {
	return &plainClient{t: t, base: a.base, admin: testAdminToken}
}

func (c *plainClient) exchange(method, path string, body any) (int, http.Header, []byte) {
	return c.exchangeWith(c.admin, method, path, body)
}

func (c *plainClient) exchangeWith(tok, method, path string, body any) (int, http.Header, []byte) {
	c.t.Helper()
	var data []byte
	if body != nil {
		data, _ = jsonMarshal(body)
	}
	status, hdr, out := doRequest(c.t, method, c.base+path, tok, data)
	return status, hdr, out
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func doRequest(t *testing.T, method, url, tok string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}
