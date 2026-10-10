// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func validInbox() *inbox.Inbox {
	in := &inbox.Inbox{Slug: "valid-slug", Name: "Valid", Verifier: "none", AgentTokenHash: []byte{1}}
	in.ApplyDefaults()
	return in
}

func TestApplyDefaults(t *testing.T) {
	in := &inbox.Inbox{}
	in.ApplyDefaults()
	if in.OnVerifyFail != inbox.OnVerifyFailReject || in.MaxBodyBytes != inbox.DefaultMaxBodyBytes ||
		string(in.VerifierConfig) != "{}" {
		t.Errorf("defaults not applied: %+v", in)
	}
	// Existing values are kept.
	in = &inbox.Inbox{OnVerifyFail: inbox.OnVerifyFailStore, MaxBodyBytes: 7, VerifierConfig: json.RawMessage(`{"a":1}`)}
	in.ApplyDefaults()
	if in.OnVerifyFail != inbox.OnVerifyFailStore || in.MaxBodyBytes != 7 || string(in.VerifierConfig) != `{"a":1}` {
		t.Errorf("defaults overwrote set values: %+v", in)
	}
}

func TestInboxValidate(t *testing.T) {
	if err := validInbox().Validate(); err != nil {
		t.Fatalf("a valid inbox failed validation: %v", err)
	}
	cases := map[string]func(*inbox.Inbox){
		"slug too short":      func(in *inbox.Inbox) { in.Slug = "ab" },
		"slug uppercase":      func(in *inbox.Inbox) { in.Slug = "ABC" },
		"slug with dot":       func(in *inbox.Inbox) { in.Slug = "a.b" },
		"name empty":          func(in *inbox.Inbox) { in.Name = "" },
		"name too long":       func(in *inbox.Inbox) { in.Name = strings.Repeat("x", 201) },
		"name bad utf-8":      func(in *inbox.Inbox) { in.Name = "\xff" },
		"verifier empty":      func(in *inbox.Inbox) { in.Verifier = "" },
		"verifier uppercase":  func(in *inbox.Inbox) { in.Verifier = "HMAC" },
		"verifier too long":   func(in *inbox.Inbox) { in.Verifier = strings.Repeat("a", 33) },
		"on_verify_fail":      func(in *inbox.Inbox) { in.OnVerifyFail = "maybe" },
		"no token hash":       func(in *inbox.Inbox) { in.AgentTokenHash = nil },
		"body limit zero":     func(in *inbox.Inbox) { in.MaxBodyBytes = 0 },
		"body limit too high": func(in *inbox.Inbox) { in.MaxBodyBytes = inbox.MaxBodyBytesCeiling + 1 },
		"negative days":       func(in *inbox.Inbox) { in.RetentionDays = -1 },
		"negative count":      func(in *inbox.Inbox) { in.RetentionMaxCount = -1 },
		"pointer no slash":    func(in *inbox.Inbox) { in.EventTypePointer = "type" },
		"pointer bad escape":  func(in *inbox.Inbox) { in.EventTypePointer = "/a~2" },
		"config array":        func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`[]`) },
		"config garbage":      func(in *inbox.Inbox) { in.VerifierConfig = json.RawMessage(`{`) },
	}
	for name, mod := range cases {
		in := validInbox()
		mod(in)
		if err := in.Validate(); !errors.Is(err, inbox.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
	for _, ok := range []string{"", "/type", "/a/b", "/a~0b", "/a~1b", "/"} {
		in := validInbox()
		in.EventTypePointer = ok
		if err := in.Validate(); err != nil {
			t.Errorf("pointer %q rejected: %v", ok, err)
		}
	}
	// A custom verifier name is valid even though it is not a built-in.
	in := validInbox()
	in.Verifier = "acme-v2"
	if err := in.Validate(); err != nil || inbox.KnownVerifier("acme-v2") {
		t.Errorf("custom verifier: validate=%v known=%v", err, inbox.KnownVerifier("acme-v2"))
	}
	if !inbox.KnownVerifier("stripe") || inbox.KnownVerifier("") {
		t.Error("KnownVerifier is wrong for built-ins")
	}
}

func TestSecretsNeverFormat(t *testing.T) {
	in := validInbox()
	in.VerifierSecret = []byte("sealed-marker-123")
	in.AgentTokenHash = []byte("hash-marker-456")

	js, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("x", "inbox", in, "secret", in.VerifierSecret)

	outputs := map[string]string{
		"json": string(js), "log": log.String(),
		"%v": fmt.Sprintf("%v", in), "%+v": fmt.Sprintf("%+v", *in), "%#v": fmt.Sprintf("%#v", *in),
		"%s": fmt.Sprintf("%s", in.VerifierSecret), //nolint:staticcheck // deliberately exercising the %s verb "%x-hex": fmt.Sprintf("%x", in.AgentTokenHash),
	}
	for name, out := range outputs {
		if name == "%x-hex" {
			continue // explicit hex of the raw bytes is a deliberate act, not a leak
		}
		if strings.Contains(out, "sealed-marker-123") || strings.Contains(out, "hash-marker-456") {
			t.Errorf("%s leaks a secret: %s", name, out)
		}
	}
	if !strings.Contains(string(js), "[redacted]") {
		t.Errorf("json output should carry the redaction marker: %s", js)
	}
	// The raw bytes are still reachable for code that needs them.
	if string(in.VerifierSecret) != "sealed-marker-123" || !bytes.Equal(in.AgentTokenHash, []byte("hash-marker-456")) {
		t.Error("SecretBytes lost its content")
	}
}

func TestQueryValidate(t *testing.T) {
	one, minus := int64(1), int64(-1)
	now := time.Now()
	invalid := map[string]inbox.Query{
		"both cursors":   {After: &one, Before: &one},
		"negative after": {After: &minus},
		"negative limit": {Limit: -5},
		"negative body":  {BodyLimit: &minus},
		"empty type":     {Types: []string{""}},
		"since>=until":   {Since: now, Until: now},
		"too many types": {Types: make([]string, inbox.MaxQueryTypes+1)},
	}
	for name, q := range invalid {
		if name == "too many types" {
			for i := range q.Types {
				q.Types[i] = fmt.Sprint(i)
			}
		}
		if err := q.Validate(); !errors.Is(err, inbox.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
	valid := []inbox.Query{{}, {After: new(int64)}, {Before: &one}, {Since: now}, {Until: now}, {Since: now, Until: now.Add(1)}}
	for i, q := range valid {
		if err := q.Validate(); err != nil {
			t.Errorf("valid query %d rejected: %v", i, err)
		}
	}
}

func TestQueryLimitsAndOrder(t *testing.T) {
	for in, want := range map[int]int{0: 50, 1: 1, 500: 500, 501: 500, 100000: 500} {
		if got := (inbox.Query{Limit: in}).EffectiveLimit(); got != want {
			t.Errorf("Limit %d -> %d, want %d", in, got, want)
		}
	}
	for in, want := range map[int]int{-1: 100, 0: 100, 5: 5, 1000: 1000, 1001: 1000} {
		if got := (inbox.ListParams{Limit: in}).EffectiveLimit(); got != want {
			t.Errorf("list limit %d -> %d, want %d", in, got, want)
		}
	}
	zero := int64(0)
	if !(inbox.Query{After: &zero}).Ascending() || (inbox.Query{}).Ascending() || (inbox.Query{Before: &zero}).Ascending() {
		t.Error("Ascending is wrong")
	}
}

func TestNormalizeTime(t *testing.T) {
	mst := time.FixedZone("MST", -7*3600)
	in := time.Date(2026, 3, 1, 5, 0, 0, 123456789, mst)
	got := inbox.NormalizeTime(in)
	want := time.Date(2026, 3, 1, 12, 0, 0, 123456000, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("NormalizeTime = %v, want %v", got, want)
	}
}

func TestRedactHeaders(t *testing.T) {
	in := http.Header{
		"Authorization":       {"secret1"},
		"authorization":       {"secret2"},
		"COOKIE":              {"secret3"},
		"Proxy-Authorization": {"secret4"},
		"X-Inbox-Token":       {"secret5"},
		"x-keep":              {"a", "b"},
		"Content-Type":        {"text/plain"},
	}
	snapshot := in.Clone()
	out := inbox.RedactHeaders(in, "x-inbox-token", " ", "")
	if !reflect.DeepEqual(in, snapshot) {
		t.Error("RedactHeaders modified its input")
	}
	want := http.Header{"X-Keep": {"a", "b"}, "Content-Type": {"text/plain"}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("RedactHeaders = %v, want %v", out, want)
	}
	if got := inbox.RedactHeaders(nil); got == nil || len(got) != 0 {
		t.Errorf("nil input should give an empty non-nil header, got %#v", got)
	}
	// The result does not share slices with the input.
	out["X-Keep"][0] = "changed"
	if in["x-keep"][0] != "a" { //nolint:staticcheck // the test built this header with a non-canonical key
		t.Error("output aliases input")
	}

	// Keys that canonicalize to the same name merge deterministically.
	for range 50 {
		got := inbox.RedactHeaders(http.Header{"x-dup": {"1"}, "X-Dup": {"2"}, "X-DUP": {"3"}})
		if !reflect.DeepEqual(got["X-Dup"], []string{"3", "2", "1"}) {
			t.Fatalf("merge order = %v, want a stable sorted-key order", got["X-Dup"])
		}
	}
}

func TestInvalidf(t *testing.T) {
	err := inbox.Invalidf("bad %s %d", "value", 7)
	if !errors.Is(err, inbox.ErrInvalid) || !strings.Contains(err.Error(), "bad value 7") {
		t.Errorf("Invalidf = %v", err)
	}
}

func TestNewMessageValidate(t *testing.T) {
	if err := (&inbox.NewMessage{}).Validate(); !errors.Is(err, inbox.ErrInvalid) {
		t.Errorf("empty method: %v", err)
	}
	if err := (&inbox.NewMessage{Method: "POST"}).Validate(); err != nil {
		t.Errorf("valid message: %v", err)
	}
}
