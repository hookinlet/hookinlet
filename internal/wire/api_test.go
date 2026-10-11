// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/internal/api"
)

// The stream and the MCP tools encode messages with these types, and the REST
// handlers build them before converting to the generated ones (internal/api,
// which is the document). A body has to reach the reader exactly as it was
// received, so these are not replaced by the generated types, whose body is a
// generic value. This test holds them to the document instead: for each, the
// generated type and the hand-written one must have the same JSON fields, and
// agree on which are omitted when empty.
func TestHandWrittenTypesHaveTheFieldsTheDocumentGives(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wire, schema any
	}{
		{"Message", Message{}, api.Message{}},
		{"Page", Page{}, api.MessagePage{}},
		{"InboxInfo", InboxInfo{}, api.InboxInfo{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s := fields(tc.wire), fields(tc.schema)
			for name, omit := range s {
				got, ok := w[name]
				switch {
				case !ok:
					t.Errorf("the document has the field %q and the type does not", name)
				case got != omit:
					t.Errorf("field %q: the document says omitempty=%v, the type %v", name, omit, got)
				}
			}
			for name := range w {
				if _, ok := s[name]; !ok && !mcpOnly[tc.name+"."+name] {
					t.Errorf("the type has the field %q and the document does not", name)
				}
			}
		})
	}
}

// mcpOnly are fields the MCP tools add to a message and the REST API never does.
// The document describes REST, and MCP's results are described in docs/mcp.md.
var mcpOnly = map[string]bool{"Message.body_truncated": true}

// fields maps each JSON field name of a struct to whether it is omitted when
// empty.
func fields(v any) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = strings.Contains(opts, "omitempty")
	}
	return out
}
