// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package specvalidate enforces the OpenAPI document (docs/openapi.json) in Go.
//
// The server embeds the document and uses this package to refuse a request that
// breaks it; the tests use the same code to check every response the server
// gives. So what holds the server to the document at run time is what the tests
// trust, and there is one checker, not two.
//
// It understands a fixed set of JSON Schema keywords and nothing else. Load
// fails on a document that uses any other keyword or format, so a schema can
// never be satisfied because part of it was ignored.
package specvalidate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Keywords is what a schema in the document may use. The first group constrains
// values; the second only describes them.
var Keywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"enum": true, "const": true, "pattern": true, "minimum": true, "maximum": true, "minLength": true,
	"maxLength": true, "minItems": true, "maxItems": true, "minProperties": true, "oneOf": true,
	"format": true, "$ref": true,
	"description": true, "example": true, "default": true, "title": true,
}

// Formats are the values of "format" that are understood.
var Formats = map[string]bool{"date-time": true, "binary": true}

// A Violation is one way a value breaks a schema.
type Violation struct {
	// Path locates the value: a property path such as "retention_days" or
	// "items[2].name"; empty for the value as a whole.
	Path string
	// Reason says what is wrong, in words that are safe to show to the caller. It
	// never repeats the value, which may be a secret.
	Reason string
}

func (v Violation) String() string {
	if v.Path == "" {
		return v.Reason
	}
	return v.Path + ": " + v.Reason
}

// Document is a loaded OpenAPI 3.1 document.
type Document struct {
	root map[string]any
	ops  []*Operation
	byID map[string]*Operation
}

// Operation is one method on one path.
type Operation struct {
	ID, Method, Path string
	Params           []Param

	doc  *Document
	body map[string]any // the request body's JSON schema, nil if it takes none
	raw  map[string]any
}

// Param is a query, path, or header parameter.
type Param struct {
	Name, In string
	Required bool
	Schema   map[string]any
	Array    bool // the schema is an array: the parameter may be repeated
}

// Load parses a document and checks that it can be enforced: every reference
// resolves, and every schema uses only the keywords and formats this package
// understands.
func Load(b []byte) (*Document, error) {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("specvalidate: the document is not JSON: %w", err)
	}
	if v, _ := root["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		return nil, fmt.Errorf("specvalidate: openapi is %q, want 3.1.x", v)
	}
	d := &Document{root: root, byID: map[string]*Operation{}}

	var problems []string
	d.walkRefs(root, "", &problems)
	// Every schema, wherever it is written, is checked for keywords.
	var visit func(v any, at string, inSchema bool)
	visit = func(v any, at string, inSchema bool) {
		switch x := v.(type) {
		case map[string]any:
			if inSchema {
				d.checkKeywords(x, at, &problems)
			}
			for k, c := range x {
				switch {
				case k == "schema" || (k == "schemas" && strings.HasSuffix(at, "components")):
					if k == "schemas" {
						for name, sc := range c.(map[string]any) {
							visit(sc, at+"/schemas/"+name, true)
						}
					} else {
						visit(c, at+"/schema", true)
					}
				default:
					visit(c, at+"/"+k, false)
				}
			}
		case []any:
			for i, c := range x {
				visit(c, fmt.Sprintf("%s[%d]", at, i), false)
			}
		}
	}
	visit(root, "", false)
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("specvalidate: the document cannot be enforced:\n  %s", strings.Join(problems, "\n  "))
	}

	paths, _ := root["paths"].(map[string]any)
	for path, item := range paths {
		for method, o := range item.(map[string]any) {
			om, ok := o.(map[string]any)
			if !ok {
				continue
			}
			op := &Operation{ID: om["operationId"].(string), Method: strings.ToUpper(method), Path: path, doc: d, raw: om}
			// Parameters may sit on the path item (not used here) or the operation.
			for _, p := range asList(om["parameters"]) {
				pm := d.Deref(p).(map[string]any)
				schema, _ := d.Deref(pm["schema"]).(map[string]any)
				prm := Param{Name: pm["name"].(string), In: pm["in"].(string), Schema: schema}
				prm.Required, _ = pm["required"].(bool)
				prm.Array = schema["type"] == "array"
				op.Params = append(op.Params, prm)
			}
			if rb, ok := d.Deref(om["requestBody"]).(map[string]any); ok {
				if js, ok := get(rb, "content", "application/json", "schema").(map[string]any); ok {
					op.body = js
				}
			}
			d.ops = append(d.ops, op)
			d.byID[op.ID] = op
		}
	}
	sort.Slice(d.ops, func(i, j int) bool { return d.ops[i].ID < d.ops[j].ID })
	return d, nil
}

func asList(v any) []any { l, _ := v.([]any); return l }

func get(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

func (d *Document) walkRefs(v any, at string, problems *[]string) {
	switch x := v.(type) {
	case map[string]any:
		if r, ok := x["$ref"].(string); ok {
			if !strings.HasPrefix(r, "#/") || d.resolve(r) == nil {
				*problems = append(*problems, fmt.Sprintf("%s: the reference %q does not resolve", at, r))
			}
		}
		for k, c := range x {
			d.walkRefs(c, at+"/"+k, problems)
		}
	case []any:
		for i, c := range x {
			d.walkRefs(c, fmt.Sprintf("%s[%d]", at, i), problems)
		}
	}
}

func (d *Document) resolve(ref string) any {
	return get(d.root, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...)
}

// checkKeywords reports keywords and formats this package would ignore.
func (d *Document) checkKeywords(sm map[string]any, at string, problems *[]string) {
	for k, v := range sm {
		if !Keywords[k] {
			*problems = append(*problems, fmt.Sprintf("%s: the schema uses the keyword %q, which is not understood", at, k))
		}
		switch k {
		case "format":
			if f, _ := v.(string); !Formats[f] {
				*problems = append(*problems, fmt.Sprintf("%s: the schema uses the format %q, which is not understood", at, v))
			}
		case "properties":
			for pn, ps := range v.(map[string]any) {
				if pm, ok := ps.(map[string]any); ok {
					d.checkKeywords(pm, at+"."+pn, problems)
				}
			}
		case "items", "additionalProperties":
			if pm, ok := v.(map[string]any); ok {
				d.checkKeywords(pm, at+"."+k, problems)
			}
		case "oneOf":
			for i, alt := range asList(v) {
				if pm, ok := alt.(map[string]any); ok {
					d.checkKeywords(pm, fmt.Sprintf("%s.oneOf[%d]", at, i), problems)
				}
			}
		}
	}
}

// Root is the parsed document. Callers must not change it.
func (d *Document) Root() map[string]any { return d.root }

// Deref follows $ref until it reaches a value that is not one.
func (d *Document) Deref(v any) any {
	for {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		r, ok := m["$ref"].(string)
		if !ok {
			return v
		}
		v = d.resolve(r)
	}
}

// Operations returns every operation, ordered by id.
func (d *Document) Operations() []*Operation { return slices.Clone(d.ops) }

// Operation returns the operation with this id, or nil.
func (d *Document) Operation(id string) *Operation { return d.byID[id] }

// Lookup finds the operation for an HTTP method and a net/http route pattern as
// the server registers it (for example "/in/{key}/{rest...}", with or without a
// leading method), or nil if the document does not describe that route.
func (d *Document) Lookup(method, pattern string) *Operation {
	if _, rest, ok := strings.Cut(pattern, " "); ok && !strings.Contains(pattern[:strings.Index(pattern, " ")], "/") {
		pattern = rest
	}
	pattern = strings.ReplaceAll(pattern, "...}", "}")
	for _, op := range d.ops {
		if op.Path == pattern && op.Method == method {
			return op
		}
	}
	return nil
}

// Response returns the document's description of one response of an operation
// ("200", "default", ...), with references followed, or nil.
func (o *Operation) Response(status string) map[string]any {
	r, _ := o.doc.Deref(get(o.raw, "responses", status)).(map[string]any)
	return r
}

// Statuses lists the statuses the operation documents, excluding "default".
func (o *Operation) Statuses() []string {
	var out []string
	for st := range o.raw["responses"].(map[string]any) {
		if st != "default" {
			out = append(out, st)
		}
	}
	sort.Strings(out)
	return out
}

// Raw is the operation as written in the document. Callers must not change it.
func (o *Operation) Raw() map[string]any { return o.raw }

// ---- checking requests ----

// CheckQuery checks a request's query string against the operation's
// parameters: every name must be one the document lists, a parameter that is not
// an array may appear once, a required one must appear, and each value must have
// the type and constraints of its schema.
func (o *Operation) CheckQuery(q url.Values) []Violation {
	var vs []Violation
	known := map[string]Param{}
	for _, p := range o.Params {
		if p.In == "query" {
			known[p.Name] = p
		}
	}
	for name, values := range q {
		p, ok := known[name]
		switch {
		case !ok:
			vs = append(vs, Violation{Path: name, Reason: "unknown parameter"})
		case !p.Array && len(values) > 1:
			vs = append(vs, Violation{Path: name, Reason: "may be given only once"})
		case p.Array:
			if n, ok := p.Schema["maxItems"].(float64); ok && float64(len(values)) > n {
				vs = append(vs, Violation{Path: name, Reason: "too many values"})
				continue
			}
			item, _ := o.doc.Deref(p.Schema["items"]).(map[string]any)
			for _, raw := range values {
				vs = append(vs, o.queryValue(name, item, raw)...)
			}
		default:
			vs = append(vs, o.queryValue(name, p.Schema, values[0])...)
		}
	}
	for name, p := range known {
		if _, ok := q[name]; p.Required && !ok {
			vs = append(vs, Violation{Path: name, Reason: "is required"})
		}
	}
	sortViolations(vs)
	return vs
}

// queryValue converts one query string value to the type its schema describes,
// and checks it. A query string only has text, and the conversion is strict: an
// integer is digits (with a leading minus if the schema allows negatives), so
// "1.5", "+1", " 1", and "1e3" are not integers.
func (o *Operation) queryValue(name string, schema map[string]any, raw string) []Violation {
	t, _ := schema["type"].(string)
	var v any = raw
	switch t {
	case "integer", "number":
		if t == "integer" && !integerText.MatchString(raw) {
			return []Violation{{Path: name, Reason: "must be a whole number"}}
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || (t == "number" && !numberText.MatchString(raw)) {
			return []Violation{{Path: name, Reason: "must be a number"}}
		}
		if t == "integer" && len(strings.TrimPrefix(raw, "-")) > 18 {
			return []Violation{{Path: name, Reason: "is too large"}}
		}
		v = f
	case "boolean":
		switch raw {
		case "true":
			v = true
		case "false":
			v = false
		default:
			return []Violation{{Path: name, Reason: "must be true or false"}}
		}
	}
	vs := o.doc.Check(schema, v, Options{})
	for i := range vs {
		vs[i].Path = name
	}
	return vs
}

var (
	integerText = regexp.MustCompile(`^-?[0-9]+$`)
	numberText  = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
)

// CheckBody checks a JSON request body against the operation's request schema.
// It fails if the body is not exactly one JSON value, and reports each way the
// value breaks the schema; objects the schema closes (additionalProperties:
// false) refuse unknown fields. An operation with no JSON request body accepts
// no check and returns nothing.
func (o *Operation) CheckBody(body []byte) []Violation {
	if o.body == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var v any
	if err := dec.Decode(&v); err != nil {
		return []Violation{{Reason: "is not valid JSON"}}
	}
	if _, err := dec.Token(); !errors.Is(err, errEOF) {
		return []Violation{{Reason: "must be a single JSON value"}}
	}
	vs := o.doc.Check(o.body, v, Options{})
	sortViolations(vs)
	return vs
}

var errEOF = func() error {
	var v any
	return json.NewDecoder(strings.NewReader("")).Decode(&v)
}()

func sortViolations(vs []Violation) {
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Path != vs[j].Path {
			return vs[i].Path < vs[j].Path
		}
		return vs[i].Reason < vs[j].Reason
	})
}

// ---- the schema checker ----

// Options changes how Check treats a value.
type Options struct {
	// ClosedObjects treats an object schema that lists properties and says nothing
	// about additionalProperties as closed. The published document leaves response
	// objects open, so that a client written from it keeps working when the server
	// adds a field; the tests check responses with this on, so the server does not
	// add one without the document.
	ClosedObjects bool
}

// Check validates v, a value decoded from JSON (maps, slices, float64, string,
// bool, nil), against schema.
func (d *Document) Check(schema, v any, opts Options) []Violation {
	return d.check(schema, v, "", opts)
}

func (d *Document) check(schema, v any, at string, opts Options) []Violation {
	schema = d.Deref(schema)
	switch sc := schema.(type) {
	case bool:
		if !sc {
			return []Violation{{Path: at, Reason: "is not allowed here"}}
		}
		return nil
	case nil:
		return []Violation{{Path: at, Reason: "has no schema"}}
	}
	sm := schema.(map[string]any)
	var vs []Violation
	bad := func(format string, a ...any) { vs = append(vs, Violation{Path: at, Reason: fmt.Sprintf(format, a...)}) }

	if c, ok := sm["const"]; ok && !jsonEqual(c, v) {
		bad("must be %v", c)
	}
	if e, ok := sm["enum"].([]any); ok && !slices.ContainsFunc(e, func(x any) bool { return jsonEqual(x, v) }) {
		parts := make([]string, len(e))
		for i, x := range e {
			parts[i] = fmt.Sprint(x)
		}
		bad("must be one of: %s", strings.Join(parts, ", "))
	}
	if one, ok := sm["oneOf"].([]any); ok {
		matches := 0
		for _, alt := range one {
			if len(d.check(alt, v, at, opts)) == 0 {
				matches++
			}
		}
		if matches != 1 {
			bad("does not match exactly one of the allowed forms")
		}
	}
	if t, ok := sm["type"]; ok {
		types := []any{t}
		if ts, ok := t.([]any); ok {
			types = ts
		}
		if !slices.ContainsFunc(types, func(ty any) bool { return typeIs(ty.(string), v) }) {
			names := make([]string, len(types))
			for i, ty := range types {
				names[i] = article(ty.(string))
			}
			return append(vs, Violation{Path: at, Reason: "must be " + strings.Join(names, " or ")})
		}
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := sm["properties"].(map[string]any)
		for _, r := range asList(sm["required"]) {
			if _, present := x[r.(string)]; !present {
				vs = append(vs, Violation{Path: join(at, r.(string)), Reason: "is required"})
			}
		}
		for k, val := range x {
			if p, ok := props[k]; ok {
				vs = append(vs, d.check(p, val, join(at, k), opts)...)
				continue
			}
			ap, stated := sm["additionalProperties"]
			if !stated && opts.ClosedObjects && props != nil {
				ap = false
			}
			switch ap := ap.(type) {
			case bool:
				if !ap {
					vs = append(vs, Violation{Path: join(at, k), Reason: "is not a known field"})
				}
			case map[string]any:
				vs = append(vs, d.check(ap, val, join(at, k), opts)...)
			}
		}
		if n, ok := sm["minProperties"].(float64); ok && float64(len(x)) < n {
			bad("must have at least %v field(s)", n)
		}
	case []any:
		if it, ok := sm["items"]; ok {
			for i, e := range x {
				vs = append(vs, d.check(it, e, fmt.Sprintf("%s[%d]", at, i), opts)...)
			}
		}
		if n, ok := sm["minItems"].(float64); ok && float64(len(x)) < n {
			bad("must have at least %v item(s)", n)
		}
		if n, ok := sm["maxItems"].(float64); ok && float64(len(x)) > n {
			bad("must have at most %v item(s)", n)
		}
	case string:
		if p, ok := sm["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			bad("is not in the required form")
		}
		if n, ok := sm["minLength"].(float64); ok && float64(len([]rune(x))) < n {
			bad("must be at least %v character(s)", n)
		}
		if n, ok := sm["maxLength"].(float64); ok && float64(len([]rune(x))) > n {
			bad("must be at most %v character(s)", n)
		}
		if sm["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, x); err != nil {
				bad("must be an RFC 3339 timestamp such as 2026-03-01T12:00:00Z")
			}
		}
	case float64:
		if m, ok := sm["minimum"].(float64); ok && x < m {
			bad("must be at least %v", m)
		}
		if m, ok := sm["maximum"].(float64); ok && x > m {
			bad("must be at most %v", m)
		}
	}
	return vs
}

func join(at, k string) string {
	if at == "" {
		return k
	}
	return at + "." + k
}

func article(t string) string {
	switch t {
	case "object", "integer":
		return "an " + t
	case "null":
		return "null"
	}
	return "a " + t
}

func typeIs(t string, v any) bool {
	switch t {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	}
	return false
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
