// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// json.Valid accepts nesting up to 10,000 levels, but common clients do not:
// Python's json module raises RecursionError at about 1,000. One such body
// embedded raw would make every page containing it unparseable, so an agent
// polling with after= could never get past it. Deeply nested bodies are
// therefore delivered as text.
func TestDeeplyNestedJSONIsDeliveredAsText(t *testing.T) {
	nest := func(depth int) []byte {
		return []byte(strings.Repeat("[", depth) + strings.Repeat("]", depth))
	}
	objects := func(depth int) []byte {
		return []byte(strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth))
	}
	tests := []struct {
		name     string
		body     []byte
		embedded bool
	}{
		{"flat", []byte(`{"a":1}`), true},
		{"array nested to the limit", nest(MaxEmbeddedJSONDepth), true},
		{"array nested one past the limit", nest(MaxEmbeddedJSONDepth + 1), false},
		{"objects nested to the limit", objects(MaxEmbeddedJSONDepth), true},
		{"objects nested past the limit", objects(MaxEmbeddedJSONDepth + 1), false},
		{"the attack: 5000 levels", nest(5000), false},
		{"9999 levels is valid JSON but not embedded", nest(9999), false},
		{"brackets inside a string do not count", []byte(`"` + strings.Repeat("[", 5000) + `"`), true},
		{"escaped quotes do not end the string early", []byte(`{"a":"\"` + strings.Repeat("[", 5000) + `"}`), true},
		{"an escaped backslash before the closing quote", []byte(`{"a":"\\"}` + ""), true},
		{"deep nesting after a string full of brackets", []byte(`["` + strings.Repeat("{", 5000) + `",` + string(nest(MaxEmbeddedJSONDepth+1)) + `]`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := encode(t, FromStored(stored(tt.body), "i", "none", true))
			keys := bodyKeys(out)
			want := "body_text"
			if tt.embedded {
				want = "body"
			}
			if len(keys) != 1 || keys[0] != want {
				t.Errorf("body fields = %v, want only %s", keys, want)
			}
			if !tt.embedded && out["body_text"] != string(tt.body) {
				t.Error("the text form does not carry the exact body")
			}
		})
	}
}

func TestJSONDepth(t *testing.T) {
	for in, want := range map[string]int{
		``: 0, `1`: 0, `"x"`: 0, `[]`: 1, `{}`: 1, `[[]]`: 2, `{"a":[{"b":[]}]}`: 4,
		`["]]]]"]`: 1, `["\"[[["]`: 1, `["\\"]`: 1, `[1,[2,[3]]]`: 3, `[[1],[2]]`: 2,
	} {
		if got := jsonDepth([]byte(in)); got != want {
			t.Errorf("jsonDepth(%q) = %d, want %d", in, got, want)
		}
	}
}

// The page budget must measure what is actually sent. Control characters are
// escaped to six bytes each, so a body of them is six times larger on the wire
// than in the database.
func TestPageBudgetCountsEncodedSize(t *testing.T) {
	s, in := newStore(t)
	const each = 600 << 10
	add(t, s, in, 10, func(i int, m *inbox.NewMessage) {
		m.Body = bytes.Repeat([]byte{0x01}, each) // valid UTF-8, not JSON: sent as body_text with \u0001 for every byte
	})
	v, _ := url.ParseQuery("after=0&limit=500")
	req, _ := parseMessages(v)
	page, err := FetchPage(t.Context(), s, in, req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	// 600 KiB of \u0001 is 3.6 MiB per message, so four fit in 16 MiB; the page
	// may exceed the budget only by the one message that crosses it.
	if len(page.Messages) > 5 {
		t.Errorf("%d messages in one page; the encoded size is %d MiB", len(page.Messages), len(raw)>>20)
	}
	if limit := PageBudget + 4<<20; len(raw) > limit {
		t.Errorf("the page is %d MiB, over the budget of %d MiB plus one message", len(raw)>>20, PageBudget>>20)
	}
	// Following next still visits every message exactly once.
	seqs, pages, _ := follow(t, s, in, "after=0&limit=500")
	if !sameSeqs(seqs, seqRange(1, 10, false)) || pages < 2 {
		t.Errorf("walk saw %v over %d pages", seqs, pages)
	}
}

// Headers, the path, and the query string are sender-controlled and stored, so
// they count toward the budget too. Without that, a summary page (which carries
// no bodies) of messages with huge headers was unbounded.
func TestPageBudgetCountsHeadersEvenForSummaries(t *testing.T) {
	s, in := newStore(t)
	const messages, headerBytes = 200, 100 << 10
	pad := strings.Repeat("h", headerBytes)
	add(t, s, in, messages, func(i int, m *inbox.NewMessage) {
		m.Body = []byte("small")
		m.Headers = map[string][]string{"X-Pad": {pad}}
	})
	for _, q := range []string{"after=0&limit=500&fields=summary", "after=0&limit=500", "limit=500&fields=summary"} {
		v, _ := url.ParseQuery(q)
		req, _ := parseMessages(v)
		page, err := FetchPage(t.Context(), s, in, req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := Marshal(page)
		if len(raw) > PageBudget+headerBytes*2 {
			t.Errorf("%s: a page of %d messages is %d MiB, over the %d MiB budget", q, len(page.Messages), len(raw)>>20, PageBudget>>20)
		}
		if len(page.Messages) >= messages {
			t.Errorf("%s: all %d messages with 100 KB headers fit in one page", q, len(page.Messages))
		}
		seqs, _, _ := follow(t, s, in, q)
		if len(seqs) != messages {
			t.Errorf("%s: a walk saw %d messages, want %d", q, len(seqs), messages)
		}
	}
}

func TestEncodedSizeEstimateIsClose(t *testing.T) {
	cases := map[string][]byte{
		"plain json":      []byte(`{"id":"evt_1","list":[1,2,3],"s":"hello"}`),
		"escapes":         []byte("a\x01b\x02\"quoted\"\\back\n\t"),
		"unicode":         []byte("héllo ☕ 日本語"),
		"line separators": []byte("a\u2028b\u2029c"),
		"binary":          {0xff, 0xfe, 0x00, 0x80, 0x81},
		"empty":           {},
	}
	for name, body := range cases {
		m := stored(body)
		m.Headers = map[string][]string{"X-Odd": {"caf\xe9", "tab\there", "quote\"q"}}
		msg := FromStored(m, "my-inbox", "stripe", true)
		raw, err := Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		est := msg.encodedSize()
		// The estimate may be generous, never short: it exists to bound the real size.
		if est < int64(len(raw)) {
			t.Errorf("%s: estimated %d bytes but the message encodes to %d", name, est, len(raw))
		}
		if est > int64(len(raw))*2+1024 {
			t.Errorf("%s: estimated %d bytes for a message that encodes to %d", name, est, len(raw))
		}
	}
}

// Compile-time use of fmt/json so the helpers above stay importable together.
var _ = fmt.Sprint
var _ = json.Valid
