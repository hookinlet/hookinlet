// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package jsonptr

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
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

// totalAlloc runs f and reports how many bytes it allocated.
func totalAlloc(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A hostile document must not cost much more memory than its own size. An
// earlier version decoded the whole document into generic values and allocated
// about 50 times the body's size (515 MiB for a 10 MiB array of zeros).
func TestMemoryStaysProportionalToTheDocument(t *testing.T) {
	const size = 10 << 20
	repeat := func(prefix, unit, suffix string) []byte {
		var b strings.Builder
		b.Grow(size + 64)
		b.WriteString(prefix)
		for b.Len() < size {
			b.WriteString(unit)
		}
		b.WriteString(suffix)
		return []byte(b.String())
	}
	cases := []struct {
		name string
		doc  []byte
		ptr  string
		max  uint64
	}{
		{"array of numbers, key pointer", repeat("[", "0,", "0]"), "/type", 2 << 20},
		{"array of empty objects, key pointer", repeat("[", "{},", "{}]"), "/type", 2 << 20},
		{"array of arrays, index pointer", repeat("[", "[1,2],", "[3]]"), "/0/0", 2 << 20},
		{"one large value to skip, then the key", repeat(`{"pad":[`, "0,", `0],"type":"x"}`), "/type", 4 * size},
		{"deeply nested", []byte(strings.Repeat(`{"a":`, 5000) + `"x"` + strings.Repeat("}", 5000)), "/a/a", 8 << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			alloc := totalAlloc(func() { _, _ = String(c.doc, c.ptr) })
			t.Logf("allocated %.1f MiB for a %d MiB document", float64(alloc)/(1<<20), len(c.doc)>>20)
			if alloc > c.max {
				t.Errorf("allocated %d MiB for a %d MiB document (limit %d MiB)", alloc>>20, len(c.doc)>>20, c.max>>20)
			}
		})
	}
}

// Work is bounded too: a document with millions of tiny values cannot make the
// lookup run for long.
func TestWorkIsBounded(t *testing.T) {
	doc := []byte("[" + strings.Repeat("0,", 3_000_000) + "0]")
	start := time.Now()
	if _, ok := String(doc, "/type"); ok {
		t.Error("found a key in an array")
	}
	if _, ok := String(doc, "/2999999"); ok {
		t.Log("an index past the work budget is not found, which is acceptable")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("two lookups took %v", d)
	}
}

func TestBudgetStillFindsEarlyValues(t *testing.T) {
	// The value is near the start; a huge tail after it must not hide it.
	doc := `{"type":"early","rest":[` + strings.Repeat("0,", 500_000) + `0]}`
	if got, ok := String([]byte(doc), "/type"); !ok || got != "early" {
		t.Errorf("String = %q, %v; want early, true", got, ok)
	}
}

// The budget is what bounds CPU time, so test it directly rather than through a
// stopwatch: on a document with millions of values the walker must give up long
// before the end.
func TestWalkerStopsAtItsBudget(t *testing.T) {
	doc := []byte("[" + strings.Repeat("0,", 3_000_000) + "0]")
	w := &walker{dec: json.NewDecoder(bytes.NewReader(doc)), budget: maxValues}
	_, found := w.lookup([]string{"type"})
	if found {
		t.Error("found a key inside an array")
	}
	if !w.exhausted || w.invalid {
		t.Errorf("exhausted=%v invalid=%v; want the budget to be what stopped it", w.exhausted, w.invalid)
	}
	if off := w.dec.InputOffset(); off > int64(len(doc))/20 {
		t.Errorf("the walker read %d of %d bytes before giving up", off, len(doc))
	}
}
