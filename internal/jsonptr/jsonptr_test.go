// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package jsonptr

import (
	"strings"
	"testing"
)

// The document from RFC 6901, section 5.
const rfcDoc = `{"foo":["bar","baz"],"":"empty-key","a/b":"slash","c%d":"percent","e^f":"caret","g|h":"pipe",
	"i\\j":"backslash","k\"l":"quote"," ":"space","m~n":"tilde","num":1,"flag":true,"nil":null,"obj":{"x":{"y":"deep"}}}`

func TestString(t *testing.T) {
	tests := []struct {
		name, doc, ptr string
		want           string
		ok             bool
	}{
		{"rfc /foo/0", rfcDoc, "/foo/0", "bar", true},
		{"rfc /foo/1", rfcDoc, "/foo/1", "baz", true},
		{"rfc empty key", rfcDoc, "/", "empty-key", true},
		{"rfc a~1b", rfcDoc, "/a~1b", "slash", true},
		{"rfc c%d", rfcDoc, "/c%d", "percent", true},
		{"rfc e^f", rfcDoc, "/e^f", "caret", true},
		{"rfc g|h", rfcDoc, "/g|h", "pipe", true},
		{"rfc backslash", rfcDoc, `/i\j`, "backslash", true},
		{"rfc quote", rfcDoc, `/k"l`, "quote", true},
		{"rfc space", rfcDoc, "/ ", "space", true},
		{"rfc m~0n", rfcDoc, "/m~0n", "tilde", true},
		{"nested", rfcDoc, "/obj/x/y", "deep", true},
		{"top-level string", `{"type":"invoice.paid"}`, "/type", "invoice.paid", true},
		{"whole document is a string", `"just a string"`, "", "just a string", true},
		{"whole document is an object", `{"a":1}`, "", "", false},

		{"number is not a string", rfcDoc, "/num", "", false},
		{"bool is not a string", rfcDoc, "/flag", "", false},
		{"null is not a string", rfcDoc, "/nil", "", false},
		{"object is not a string", rfcDoc, "/obj", "", false},
		{"array is not a string", rfcDoc, "/foo", "", false},
		{"missing key", rfcDoc, "/nope", "", false},
		{"missing nested key", rfcDoc, "/obj/x/z", "", false},
		{"index out of range", rfcDoc, "/foo/2", "", false},
		{"index leading zero", rfcDoc, "/foo/01", "", false},
		{"index negative", rfcDoc, "/foo/-1", "", false},
		{"index dash", rfcDoc, "/foo/-", "", false},
		{"index not a number", rfcDoc, "/foo/x", "", false},
		{"index empty", rfcDoc, "/foo/", "", false},
		{"index huge", rfcDoc, "/foo/99999999999999999999", "", false},
		{"step into a string", `{"a":"s"}`, "/a/b", "", false},
		{"no leading slash", rfcDoc, "foo", "", false},
		{"bad escape ~2", rfcDoc, "/a~2b", "", false},
		{"trailing tilde", rfcDoc, "/a~", "", false},
		{"invalid json", `{"type":`, "/type", "", false},
		{"empty document", ``, "/type", "", false},
		{"not json", `plain text`, "/type", "", false},
		{"trailing garbage is ignored after the first value", `{"type":"x"} trailing`, "/type", "x", true},
		{"duplicate keys: the last wins, like encoding/json", `{"type":"a","type":"b"}`, "/type", "b", true},
		{"unicode", `{"type":"café ☕"}`, "/type", "café ☕", true},
		{"escaped unicode", `{"type":"é"}`, "/type", "é", true},
		{"too deep", `{}`, strings.Repeat("/a", 40), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := String([]byte(tt.doc), tt.ptr)
			if got != tt.want || ok != tt.ok {
				t.Errorf("String(%q) = %q, %v; want %q, %v", tt.ptr, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func FuzzString(f *testing.F) {
	for _, seed := range []string{"", "/", "/a", "/a/0", "/~0~1", "/a~", "//", "/" + strings.Repeat("a/", 50)} {
		f.Add(`{"a":["x"],"":"e"}`, seed)
	}
	f.Add(`[[["deep"]]]`, "/0/0/0")
	f.Fuzz(func(t *testing.T, doc, ptr string) {
		// Must never panic, whatever the document and pointer.
		_, _ = String([]byte(doc), ptr)
	})
}
