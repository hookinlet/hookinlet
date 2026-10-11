// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package specvalidate

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// doc is a small document that uses every keyword the package understands.
const doc = `{
 "openapi": "3.1.0",
 "info": {"title": "t", "version": "1"},
 "paths": {
  "/things/{id}": {
   "get": {
    "operationId": "getThing",
    "parameters": [
     {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
     {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 10}},
     {"name": "ratio", "in": "query", "schema": {"type": "number"}},
     {"name": "flag", "in": "query", "schema": {"type": "boolean"}},
     {"name": "since", "in": "query", "schema": {"type": "string", "format": "date-time"}},
     {"name": "kind", "in": "query", "schema": {"type": "string", "enum": ["a", "b"]}},
     {"name": "types", "in": "query", "schema": {"type": "array", "maxItems": 2, "items": {"type": "string", "maxLength": 3}}},
     {"name": "need", "in": "query", "required": true, "schema": {"type": "string"}}
    ],
    "responses": {"200": {"description": "ok"}, "default": {"description": "err"}}
   }
  },
  "/things": {
   "post": {
    "operationId": "makeThing",
    "requestBody": {"x-max-bytes": 1000, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}},
    "responses": {"201": {"$ref": "#/components/responses/Made"}, "400": {"description": "bad"}}
   },
   "get": {"operationId": "listThings", "responses": {"200": {"description": "ok"}}}
  }
 },
 "components": {
  "responses": {"Made": {"description": "made"}},
  "schemas": {
   "Thing": {
    "type": "object", "required": ["name"], "minProperties": 1, "additionalProperties": false,
    "properties": {
     "name": {"type": "string", "minLength": 2, "maxLength": 5, "pattern": "^[a-z]+$"},
     "n": {"type": "integer", "minimum": 0, "maximum": 9},
     "tags": {"type": "array", "minItems": 1, "maxItems": 2, "items": {"type": "string"}},
     "when": {"type": "string", "format": "date-time"},
     "kind": {"const": "thing"},
     "id": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
     "maybe": {"type": ["string", "null"]},
     "extra": {"type": "object", "additionalProperties": {"type": "integer"}}
    }
   }
  }
 }
}`

func load(t *testing.T, s string) *Document {
	t.Helper()
	d, err := Load([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func reasons(vs []Violation) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.String()
	}
	return strings.Join(out, "; ")
}

func TestLoadRefusesWhatItCouldNotEnforce(t *testing.T) {
	for name, tc := range map[string]struct{ mutate, want string }{
		"unknown keyword":    {`"type": "string", "uniqueItems": true`, `"uniqueItems"`},
		"allOf":              {`"allOf": []`, `"allOf"`},
		"unknown format":     {`"type": "string", "format": "email"`, `"email"`},
		"dangling reference": {`"$ref": "#/components/schemas/Nope"`, `does not resolve`},
		"external reference": {`"$ref": "other.json#/X"`, `does not resolve`},
	} {
		t.Run(name, func(t *testing.T) {
			s := strings.Replace(doc, `"kind": {"const": "thing"}`, `"kind": {`+tc.mutate+`}`, 1)
			_, err := Load([]byte(s))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error mentioning %s", err, tc.want)
			}
		})
	}
	for name, s := range map[string]string{
		"not json":       `{`,
		"wrong major":    `{"openapi":"3.0.3","paths":{}}`,
		"no version":     `{"paths":{}}`,
		"nested alien":   strings.Replace(doc, `"items": {"type": "string", "maxLength": 3}`, `"items": {"type": "string", "not": {}}`, 1),
		"alien in oneOf": strings.Replace(doc, `{"type": "integer"}]`, `{"type": "integer", "multipleOf": 2}]`, 1),
	} {
		if _, err := Load([]byte(s)); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}
}

func TestTheRealDocumentLoads(t *testing.T) {
	b, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Skip("no document next to this package")
	}
	d := load(t, string(b))
	if len(d.Operations()) == 0 {
		t.Fatal("no operations")
	}
}

func TestOperationsAndLookup(t *testing.T) {
	d := load(t, doc)
	var ids []string
	for _, op := range d.Operations() {
		ids = append(ids, op.ID)
	}
	if got := strings.Join(ids, ","); got != "getThing,listThings,makeThing" {
		t.Errorf("operations = %s", got)
	}
	if d.Operation("makeThing") == nil || d.Operation("nope") != nil {
		t.Error("Operation by id")
	}
	for _, tc := range []struct {
		method, pattern, want string
	}{
		{"GET", "/things/{id}", "getThing"},
		{"GET", "GET /things/{id}", "getThing"},
		{"POST", "POST /things", "makeThing"},
		{"GET", "/things", "listThings"},
		{"DELETE", "/things", ""},
		{"GET", "/other", ""},
	} {
		op := d.Lookup(tc.method, tc.pattern)
		got := ""
		if op != nil {
			got = op.ID
		}
		if got != tc.want {
			t.Errorf("Lookup(%s, %s) = %q, want %q", tc.method, tc.pattern, got, tc.want)
		}
	}
	if got := d.Lookup("GET", "/things/{id...}"); got == nil {
		t.Error("a {name...} wildcard should match {name}")
	}
	op := d.Operation("makeThing")
	if op.Response("201") == nil || op.Response("400") == nil || op.Response("500") != nil {
		t.Error("Response")
	}
	if got := strings.Join(d.Operation("getThing").Statuses(), ","); got != "200" {
		t.Errorf("Statuses = %q (default is not a status)", got)
	}
}

func TestCheckQuery(t *testing.T) {
	op := load(t, doc).Operation("getThing")
	q := func(s string) url.Values {
		v, err := url.ParseQuery(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ok := []string{
		"need=x", "need=x&limit=1", "need=x&limit=10",
		"need=x&ratio=1.5", "need=x&ratio=-2", "need=x&flag=true", "need=x&flag=false",
		"need=x&since=2026-01-02T03:04:05Z", "need=x&kind=a",
		"need=x&types=a&types=bcd", "need=",
	}
	for _, s := range ok {
		if vs := op.CheckQuery(q(s)); len(vs) != 0 {
			t.Errorf("%q refused: %s", s, reasons(vs))
		}
	}
	bad := map[string]string{
		"":                                  "need: is required",
		"need=x&nope=1":                     "nope: unknown parameter",
		"need=x&limit=1&limit=2":            "limit: may be given only once",
		"need=x&limit=1.5":                  "limit: must be a whole number",
		"need=x&limit=+1":                   "limit: must be a whole number",
		"need=x&limit=%201":                 "limit: must be a whole number",
		"need=x&limit=1e3":                  "limit: must be a whole number",
		"need=x&limit=abc":                  "limit: must be a whole number",
		"need=x&limit=":                     "limit: must be a whole number",
		"need=x&limit=-0":                   "limit: must be at least 1",
		"need=x&need=":                      "need: may be given only once",
		"need=x&limit=0":                    "limit: must be at least 1",
		"need=x&limit=11":                   "limit: must be at most 10",
		"need=x&limit=99999999999999999999": "limit: is too large",
		"need=x&ratio=1e3":                  "ratio: must be a number",
		"need=x&ratio=NaN":                  "ratio: must be a number",
		"need=x&ratio=Inf":                  "ratio: must be a number",
		"need=x&flag=yes":                   "flag: must be true or false",
		"need=x&flag=TRUE":                  "flag: must be true or false",
		"need=x&since=yesterday":            "since: must be an RFC 3339 timestamp",
		"need=x&kind=c":                     "kind: must be one of: a, b",
		"need=x&types=a&types=b&types=c":    "types: too many values",
		"need=x&types=abcd":                 "types: must be at most 3 character(s)",
	}
	for s, want := range bad {
		vs := op.CheckQuery(q(s))
		if len(vs) == 0 || !strings.Contains(reasons(vs), want) {
			t.Errorf("%q: got %q, want it to contain %q", s, reasons(vs), want)
		}
	}
	// Path parameters are not query parameters.
	if vs := op.CheckQuery(q("need=x&id=1")); len(vs) == 0 {
		t.Error("a path parameter was accepted in the query")
	}
	// The same input always gives the same order.
	a := reasons(op.CheckQuery(q("a=1&b=2&c=3")))
	for range 20 {
		if b := reasons(op.CheckQuery(q("a=1&b=2&c=3"))); a != b {
			t.Fatalf("order changed: %q vs %q", a, b)
		}
	}
}

func TestCheckBody(t *testing.T) {
	op := load(t, doc).Operation("makeThing")
	ok := []string{
		`{"name":"abc"}`,
		`{"name":"abc","n":0,"tags":["x"],"when":"2026-01-02T03:04:05.123Z","kind":"thing","id":"s","maybe":null,"extra":{"a":1}}`,
		`{"name":"abc","id":7,"maybe":"s"}`,
		` {"name":"abc"} `,
	}
	for _, s := range ok {
		if vs := op.CheckBody([]byte(s)); len(vs) != 0 {
			t.Errorf("%s refused: %s", s, reasons(vs))
		}
	}
	bad := map[string]string{
		``:                                    "is not valid JSON",
		`{`:                                   "is not valid JSON",
		`{"name":"abc"} {"name":"abd"}`:       "must be a single JSON value",
		`{"name":"abc"}x`:                     "must be a single JSON value",
		`[]`:                                  "must be an object",
		`"s"`:                                 "must be an object",
		`null`:                                "must be an object",
		`{}`:                                  "name: is required",
		`{"name":"abc","zzz":1}`:              "zzz: is not a known field",
		`{"name":"a"}`:                        "name: must be at least 2 character(s)",
		`{"name":"abcdef"}`:                   "name: must be at most 5 character(s)",
		`{"name":"ABC"}`:                      "name: is not in the required form",
		`{"name":5}`:                          "name: must be a string",
		`{"name":"abc","n":1.5}`:              "n: must be an integer",
		`{"name":"abc","n":-1}`:               "n: must be at least 0",
		`{"name":"abc","n":10}`:               "n: must be at most 9",
		`{"name":"abc","n":"1"}`:              "n: must be an integer",
		`{"name":"abc","tags":[]}`:            "tags: must have at least 1 item(s)",
		`{"name":"abc","tags":["a","b","c"]}`: "tags: must have at most 2 item(s)",
		`{"name":"abc","tags":["a",2]}`:       "tags[1]: must be a string",
		`{"name":"abc","when":"soon"}`:        "when: must be an RFC 3339 timestamp",
		`{"name":"abc","kind":"other"}`:       "kind: must be thing",
		`{"name":"abc","id":true}`:            "id: does not match exactly one of the allowed forms",
		`{"name":"abc","maybe":1}`:            "maybe: must be a string or null",
		`{"name":"abc","extra":{"a":"x"}}`:    "extra.a: must be an integer",
	}
	for s, want := range bad {
		vs := op.CheckBody([]byte(s))
		if len(vs) == 0 || !strings.Contains(reasons(vs), want) {
			t.Errorf("%s: got %q, want it to contain %q", s, reasons(vs), want)
		}
	}
	// minProperties on a schema with nothing required.
	d := load(t, strings.Replace(doc, `"required": ["name"], `, ``, 1))
	if vs := d.Operation("makeThing").CheckBody([]byte(`{}`)); !strings.Contains(reasons(vs), "at least 1 field(s)") {
		t.Errorf("minProperties: %q", reasons(vs))
	}
	// An operation without a JSON body checks nothing.
	if vs := load(t, doc).Operation("listThings").CheckBody([]byte(`not json`)); vs != nil {
		t.Errorf("a body-less operation reported %v", vs)
	}
	// A violation never repeats the value, which may be a secret.
	for _, vs := range [][]Violation{op.CheckBody([]byte(`{"name":"SECRETVALUE"}`))} {
		if strings.Contains(reasons(vs), "SECRETVALUE") {
			t.Errorf("a reason repeats the value: %s", reasons(vs))
		}
	}
}

func TestClosedObjectsOnlyClosesObjectsThatListProperties(t *testing.T) {
	d := load(t, `{"openapi":"3.1.0","paths":{},"components":{"schemas":{}}}`)
	listed := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	free := map[string]any{"type": "object"}
	v := map[string]any{"a": "x", "b": 1.0}
	if vs := d.Check(listed, v, Options{}); len(vs) != 0 {
		t.Errorf("open object refused: %s", reasons(vs))
	}
	if vs := d.Check(listed, v, Options{ClosedObjects: true}); !strings.Contains(reasons(vs), "b: is not a known field") {
		t.Errorf("closed object accepted an unknown field: %q", reasons(vs))
	}
	if vs := d.Check(free, v, Options{ClosedObjects: true}); len(vs) != 0 {
		t.Errorf("a free-form object was closed: %s", reasons(vs))
	}
	open := map[string]any{"type": "object", "additionalProperties": true, "properties": listed["properties"]}
	if vs := d.Check(open, v, Options{ClosedObjects: true}); len(vs) != 0 {
		t.Errorf("an explicitly open object was closed: %s", reasons(vs))
	}
}

func TestBooleanSchemasAndMissingSchemas(t *testing.T) {
	d := load(t, `{"openapi":"3.1.0","paths":{}}`)
	if len(d.Check(false, 1.0, Options{})) == 0 {
		t.Error("false schema accepted a value")
	}
	if len(d.Check(true, 1.0, Options{})) != 0 {
		t.Error("true schema refused a value")
	}
	if len(d.Check(nil, 1.0, Options{})) == 0 {
		t.Error("a missing schema accepted a value")
	}
}

func TestPathsOfNestedViolations(t *testing.T) {
	op := load(t, doc).Operation("makeThing")
	vs := op.CheckBody([]byte(`{"name":"abc","tags":["a",2],"extra":{"k":"x"}}`))
	if got := reasons(vs); got != "extra.k: must be an integer; tags[1]: must be a string" {
		t.Errorf("got %q", got)
	}
}

func TestLoadRefusesSchemasItCouldNotApplyFaithfully(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"a constraint beside a $ref": {`"name": {"type": "string", "minLength": 2,`, `"name": {"$ref": "#/components/schemas/Thing", "minLength": 2, "xx": {`, "beside a $ref"},
		"a lookahead":                {`"pattern": "^[a-z]+$"`, `"pattern": "^(?=a)[a-z]+$"`, "not a valid RE2"},
	} {
		s := strings.Replace(doc, tc.from, tc.to, 1)
		if name == "a constraint beside a $ref" {
			s = strings.Replace(doc, `"kind": {"const": "thing"}`, `"kind": {"$ref": "#/components/schemas/Thing", "minProperties": 3}`, 1)
		}
		if _, err := Load([]byte(s)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Load = %v, want an error containing %q", name, err, tc.want)
		}
	}
	// An annotation beside a $ref is harmless.
	s := strings.Replace(doc, `"kind": {"const": "thing"}`, `"kind": {"$ref": "#/components/schemas/Thing", "description": "x"}`, 1)
	if _, err := Load([]byte(s)); err != nil {
		t.Errorf("an annotation beside a $ref: %v", err)
	}
}

func TestAReferenceCycleEndsInsteadOfHanging(t *testing.T) {
	s := `{"openapi":"3.1.0","paths":{},"components":{"schemas":{"A":{"$ref":"#/components/schemas/B"},"B":{"$ref":"#/components/schemas/A"}}}}`
	d, err := Load([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	if vs := d.Check(map[string]any{"$ref": "#/components/schemas/A"}, 1.0, Options{}); len(vs) == 0 {
		t.Error("a cyclic reference accepted a value")
	}
}

func TestIntegerQueryValuesUpToInt64AreAccepted(t *testing.T) {
	op := load(t, doc).Operation("getThing")
	for _, tc := range []struct {
		v    string
		want string
	}{{"9223372036854775807", "limit: must be at most 10"}, {"9223372036854775808", "limit: is too large"}, {"-9223372036854775808", "limit: must be at least 1"}} {
		vs := op.CheckQuery(url.Values{"need": {"x"}, "limit": {tc.v}})
		if !strings.Contains(reasons(vs), tc.want) {
			t.Errorf("limit=%s: %q, want %q", tc.v, reasons(vs), tc.want)
		}
	}
}

func TestLoadRefusesOperationsThatLeaveWhatTheServerWouldDecideByDefault(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"a JSON body with no limit":      {`"x-max-bytes": 1000, `, ``, "x-max-bytes"},
		"a path with two credentials":    {`"operationId": "listThings",`, `"operationId": "listThings", "security": [{"adminToken": []}],`, "same credential"},
		"an agent route that names none": {`"operationId": "getThing",`, `"operationId": "getThing", "security": [{"agentToken": []}],`, "x-inbox"},
	} {
		s := strings.Replace(doc, tc.from, tc.to, 1)
		if _, err := Load([]byte(s)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Load = %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func TestSecurityPathsAndRoutes(t *testing.T) {
	s := strings.Replace(doc, `"operationId": "getThing",`, `"operationId": "getThing", "security": [{"agentToken": []}, {"adminToken": []}],`, 1)
	s = strings.Replace(s, `{"name": "id", "in": "path", "required": true,`, `{"name": "id", "in": "path", "required": true, "x-inbox": true,`, 1)
	d := load(t, s)
	op := d.Operation("getThing")
	if op.Security() != Agent {
		t.Errorf("security = %v", op.Security())
	}
	if name, ok := op.InboxParam(); !ok || name != "id" {
		t.Errorf("inbox param = %q, %v", name, ok)
	}
	if d.Operation("makeThing").Security() != Open {
		t.Error("an operation with no security needs none")
	}
	var found bool
	for _, r := range d.Routes() {
		if r.Path == "/things" {
			found = true
			if len(r.Methods) != 2 || r.Methods[0] != "GET" || r.Methods[1] != "POST" {
				t.Errorf("methods = %v", r.Methods)
			}
		}
	}
	if !found {
		t.Error("no route for /things")
	}
	if vs := op.CheckPath(func(string) string { return "01" }); len(vs) != 0 {
		t.Errorf("a string path parameter refused: %v", vs)
	}
}

func TestTheReasonForAWrongTypeOnlyReplacesTheTypeMessageOfAnArray(t *testing.T) {
	d := load(t, `{"openapi":"3.1.0","paths":{}}`)
	sc := map[string]any{"type": "object", "x-reason": "no batches"}
	if vs := d.Check(sc, []any{}, Options{}); len(vs) != 1 || vs[0].Reason != "no batches" {
		t.Errorf("an array: %v", vs)
	}
	if vs := d.Check(sc, 5.0, Options{}); len(vs) != 1 || vs[0].Reason != "must be an object" {
		t.Errorf("a number was given the batch message: %v", vs)
	}
}
