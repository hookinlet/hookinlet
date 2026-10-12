// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/internal/api"
	"github.com/hookinlet/hookinlet/internal/wire"
)

// jsonFields lists the JSON names of a struct's fields, looking into the fields of
// the given nested struct types as if they were inlined.
func jsonFields(t reflect.Type, inline ...reflect.Type) []string {
	var names []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		inlined := false
		for _, in := range inline {
			if f.Type == in {
				names = append(names, jsonFields(in)...)
				inlined = true
			}
		}
		if !inlined {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// A message is written three ways: by the REST API (and the stream, which sends the
// same JSON), by wire, which builds it, and by the MCP tools, which move the sender's
// fields under one key. They are separate types, and this is what stops one from
// gaining or losing a field without the others.
func TestTheMessageTypesCarryTheSameFields(t *testing.T) {
	rest := jsonFields(reflect.TypeFor[api.Message]())
	built := jsonFields(reflect.TypeFor[wire.Message]())
	mcp := jsonFields(reflect.TypeFor[mcpMessage](), reflect.TypeFor[mcpUntrusted]())
	// "untrusted" is the key that holds the sender's fields, not a field of the message.
	mcp = slicesDelete(mcp, "untrusted")
	// body_truncated is set only where a size cap applies, which the MCP tools set and
	// the REST API never does; the REST document does not have it. In the MCP message
	// it is a server flag and sits at the top level, which is checked here.
	built, mcp = slicesDelete(built, "body_truncated"), slicesDelete(mcp, "body_truncated")
	if !slices.Contains(jsonFields(reflect.TypeFor[mcpMessage]()), "body_truncated") {
		t.Error("body_truncated must be a top-level, server-set field of the MCP message")
	}
	for _, n := range jsonFields(reflect.TypeFor[mcpUntrusted]()) {
		if n == "body_truncated" {
			t.Error("body_truncated is a server flag and must not sit under untrusted")
		}
	}
	if !reflect.DeepEqual(rest, built) {
		t.Errorf("the REST message and wire.Message differ:\n rest %v\n wire %v", rest, built)
	}
	if !reflect.DeepEqual(rest, mcp) {
		t.Errorf("the REST message and the MCP message differ:\n rest %v\n mcp  %v", rest, mcp)
	}
}

func slicesDelete(s []string, name string) []string {
	var out []string
	for _, x := range s {
		if x != name {
			out = append(out, x)
		}
	}
	return out
}
