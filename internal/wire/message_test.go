// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func stored(body []byte) *inbox.Message {
	return &inbox.Message{
		Seq: 7, ReceivedAt: time.Date(2026, 3, 1, 12, 0, 0, 123456000, time.UTC), Method: "POST", PathSuffix: "a/b", Query: "x=1",
		Headers: http.Header{"X-A": {"1", "2"}}, ContentType: "application/json", Size: int64(len(body)), Verified: true,
		DeliveryID: "d-1", EventType: "ping", SourceIP: "203.0.113.5", Body: body,
	}
}

// encode runs the message through the same encoder the API uses and decodes it
// generically, so the tests see what a client sees.
func encode(t *testing.T, m Message) map[string]any {
	t.Helper()
	raw, err := Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("the encoded message is not valid JSON: %s", raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func bodyKeys(out map[string]any) []string {
	var keys []string
	for _, k := range []string{"body", "body_text", "body_base64"} {
		if _, ok := out[k]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}

func TestBodyEncoding(t *testing.T) {
	big := strings.Repeat(`{"k":"v"},`, 10000)
	tests := []struct {
		name string
		body []byte
		key  string
		want any // the decoded value of that key
	}{
		{"object", []byte(`{"id":"evt_1","n":[1,2,{"x":null}]}`), "body", map[string]any{"id": "evt_1", "n": []any{1.0, 2.0, map[string]any{"x": nil}}}},
		{"array", []byte(`[1,"a",true]`), "body", []any{1.0, "a", true}},
		{"string", []byte(`"just a string"`), "body", "just a string"},
		{"number", []byte(`42`), "body", 42.0},
		{"true", []byte(`true`), "body", true},
		{"null", []byte(`null`), "body", nil},
		{"json with surrounding whitespace", []byte(" \n{\"a\":1}\r\n"), "body", map[string]any{"a": 1.0}},
		{"json with unicode and escapes", []byte(`{"s":"café \"q\" \\ ☕"}`), "body", map[string]any{"s": `café "q" \ ☕`}},
		{"json with html characters", []byte(`{"s":"<script>&</script>"}`), "body", map[string]any{"s": "<script>&</script>"}},
		{"empty body", []byte(""), "body_text", ""},
		{"whitespace only", []byte("  \n"), "body_text", "  \n"},
		{"plain text", []byte("hello world"), "body_text", "hello world"},
		{"form encoded", []byte("a=1&b=%20x"), "body_text", "a=1&b=%20x"},
		{"almost json", []byte(`{"a":1,}`), "body_text", `{"a":1,}`},
		{"truncated json", []byte(`{"a":`), "body_text", `{"a":`},
		{"single quoted", []byte(`{'a':1}`), "body_text", `{'a':1}`},
		{"text with control characters", []byte("line1\r\nline2\t\x01"), "body_text", "line1\r\nline2\t\x01"},
		{"text with U+2028", []byte("a b"), "body_text", "a b"},
		{"utf-8 bom then json", []byte("\xef\xbb\xbf{\"a\":1}"), "body_text", "\xef\xbb\xbf{\"a\":1}"},
		{"binary", []byte{0, 1, 2, 0xff, 0xfe}, "body_base64", base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff, 0xfe})},
		{"lone continuation byte", []byte{0x80}, "body_base64", "gA=="},
		{"json with invalid utf-8 inside a string", []byte("{\"a\":\"\xff\"}"), "body_base64", base64.StdEncoding.EncodeToString([]byte("{\"a\":\"\xff\"}"))},
		{"overlong utf-8", []byte{0xc0, 0xaf}, "body_base64", "wK8="},
		{"surrogate half in utf-8", []byte{0xed, 0xa0, 0x80}, "body_base64", "7aCA"},
		{"large json array", []byte("[" + big + "1]"), "body", nil}, // value checked separately
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := encode(t, FromStored(stored(tt.body), "inbox-x", "none", true))
			if keys := bodyKeys(out); len(keys) != 1 || keys[0] != tt.key {
				t.Fatalf("body fields present = %v, want only %s", keys, tt.key)
			}
			if tt.name == "large json array" {
				return
			}
			if got := out[tt.key]; !equalJSON(got, tt.want) {
				t.Errorf("%s = %#v, want %#v", tt.key, got, tt.want)
			}
		})
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// A client that decodes body_base64 must get back exactly the bytes sent, and
// one that reads body_text must get the same text.
func TestBodiesRoundTripExactly(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	for name, body := range map[string][]byte{"all bytes": all, "nul": {0}, "text": []byte("héllo ☕"), "crlf": []byte("a\r\nb")} {
		out := encode(t, FromStored(stored(body), "i", "none", true))
		switch {
		case out["body_base64"] != nil:
			got, err := base64.StdEncoding.DecodeString(out["body_base64"].(string))
			if err != nil || !bytes.Equal(got, body) {
				t.Errorf("%s: base64 does not round trip", name)
			}
		case out["body_text"] != nil:
			if out["body_text"].(string) != string(body) {
				t.Errorf("%s: text does not round trip", name)
			}
		default:
			t.Errorf("%s: no body field: %v", name, out)
		}
	}
}

func TestJSONBodyIsEmbeddedNotEscaped(t *testing.T) {
	// The body is the sender's JSON, not a string containing it.
	raw, err := Marshal(FromStored(stored([]byte(`{"a":"<b>&"}`)), "i", "none", true))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"body":{"a":"<b>&"}`)) {
		t.Errorf("JSON body was escaped or re-encoded: %s", raw)
	}
}

func TestSummaryHasNoBodyFields(t *testing.T) {
	m := stored([]byte(`{"a":1}`))
	for name, withBody := range map[string]bool{"summary": false} {
		out := encode(t, FromStored(m, "i", "none", withBody))
		if keys := bodyKeys(out); len(keys) != 0 {
			t.Errorf("%s: body fields present: %v", name, keys)
		}
		if out["size"].(float64) != float64(len(`{"a":1}`)) {
			t.Errorf("%s: size = %v", name, out["size"])
		}
	}
	// A message whose body was withheld by the store carries no body either.
	withheld := stored(nil)
	withheld.Body = nil
	withheld.Size = 5000
	out := encode(t, FromStored(withheld, "i", "none", true))
	if keys := bodyKeys(out); len(keys) != 0 || out["size"].(float64) != 5000 {
		t.Errorf("withheld body: fields %v, size %v", keys, out["size"])
	}
}

func TestMessageFields(t *testing.T) {
	out := encode(t, FromStored(stored([]byte("x")), "my-inbox", "stripe", true))
	want := map[string]any{
		"seq": 7.0, "inbox": "my-inbox", "received_at": "2026-03-01T12:00:00.123456Z", "method": "POST", "path": "a/b", "query": "x=1",
		"content_type": "application/json", "size": 1.0, "verified": true, "verifier": "stripe",
		"delivery_id": "d-1", "event_type": "ping", "source_ip": "203.0.113.5", "verify_error": nil,
	}
	for k, v := range want {
		if got, ok := out[k]; !ok || !equalJSON(got, v) {
			t.Errorf("%s = %#v (present=%v), want %#v", k, got, ok, v)
		}
	}
	if h := out["headers"].(map[string]any)["X-A"]; !equalJSON(h, []any{"1", "2"}) {
		t.Errorf("headers = %v", out["headers"])
	}
}

func TestAbsentFieldsAreNullAndHeadersAreNeverNull(t *testing.T) {
	m := &inbox.Message{Seq: 1, Method: "PUT", Body: []byte{}}
	out := encode(t, FromStored(m, "i", "none", true))
	for _, k := range []string{"delivery_id", "event_type", "source_ip", "verify_error"} {
		if v, ok := out[k]; !ok || v != nil {
			t.Errorf("%s = %#v (present=%v), want an explicit null", k, v, ok)
		}
	}
	if h, ok := out["headers"].(map[string]any); !ok || len(h) != 0 {
		t.Errorf("headers = %#v, want {}", out["headers"])
	}
	if out["body_text"] != "" {
		t.Errorf("an empty body should be body_text \"\", got %v", out)
	}
}

func TestUnverifiedMessageCarriesTheReason(t *testing.T) {
	m := stored([]byte("x"))
	m.Verified, m.VerifyError = false, "signature mismatch"
	out := encode(t, FromStored(m, "i", "github", true))
	if out["verified"] != false || out["verify_error"] != "signature mismatch" {
		t.Errorf("verified=%v verify_error=%v", out["verified"], out["verify_error"])
	}
}

func TestEncodingDoesNotShareMemoryWithTheStoredMessage(t *testing.T) {
	m := stored([]byte(`{"a":1}`))
	enc := FromStored(m, "i", "none", true)
	m.Body[2] = 'Z'
	if string(enc.Body) != `{"a":1}` {
		t.Errorf("the encoded body changed with the stored one: %s", enc.Body)
	}
}
