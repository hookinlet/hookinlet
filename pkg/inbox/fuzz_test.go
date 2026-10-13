// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// The fuzz targets here run their seeds in a normal `go test`. To fuzz one:
//
//	go test -run='^$' -fuzz=FuzzRedactHeaders -fuzztime=90s ./pkg/inbox

// FuzzRedactHeaders: whatever names a request arrives with, the credentials
// never survive, nothing else is lost, and the input is left alone.
func FuzzRedactHeaders(f *testing.F) {
	f.Add("Authorization", "Bearer x", "x-inbox-token", "secret", "x-inbox-token")
	f.Add("COOKIE", "a=b", "cookie", "c=d", "")
	f.Add("proxy-authorization", "p", "X-Other", "o", " X-Other ")
	f.Add("", "", "\xff", "\x00", "\xff")
	f.Fuzz(func(t *testing.T, k1, v1, k2, v2, extra string) {
		in := http.Header{k1: {v1}, k2: {v2}}
		if k1 == k2 {
			in = http.Header{k1: {v1, v2}}
		}
		before := len(in)
		out := inbox.RedactHeaders(in, extra)
		if out == nil || len(in) != before {
			t.Fatal("nil result or modified input")
		}
		drop := map[string]bool{"Authorization": true, "Cookie": true, "Proxy-Authorization": true}
		if e := strings.TrimSpace(extra); e != "" {
			drop[http.CanonicalHeaderKey(e)] = true
		}
		for k := range out {
			if drop[k] || drop[http.CanonicalHeaderKey(k)] {
				t.Fatalf("kept the header %q", k)
			}
		}
		total := 0
		for k, vs := range in {
			if !drop[http.CanonicalHeaderKey(k)] {
				total += len(vs)
			}
		}
		kept := 0
		for _, vs := range out {
			kept += len(vs)
		}
		if kept != total {
			t.Fatalf("kept %d values, want %d", kept, total)
		}
	})
}

// FuzzInboxValidate: Validate never panics, and an inbox it accepts has the
// shape the rest of the system relies on.
func FuzzInboxValidate(f *testing.F) {
	f.Add("my-inbox", "key_123", "Name", "stripe", "{}", "/type", int64(1000), 30, int64(0))
	f.Add("", "", "", "", "", "", int64(0), -1, int64(-1))
	f.Add("A_B", "k\n", "\xff", "UPPER", "[]", "/a~", int64(1<<62), 1<<30, int64(1<<62))
	f.Add("abc", "abc", "n", "x", "null", "/~0~1/", int64(1), 0, int64(1))
	f.Fuzz(func(t *testing.T, slug, key, name, verifier, cfg, pointer string, maxBody int64, days int, maxCount int64) {
		in := &inbox.Inbox{
			Slug: slug, IngestKey: key, Name: name, Verifier: verifier, VerifierConfig: json.RawMessage(cfg),
			MaxBodyBytes: maxBody, RetentionDays: days, RetentionMaxCount: maxCount, EventTypePointer: pointer,
			OnVerifyFail: inbox.OnVerifyFailReject,
		}
		if in.Validate() != nil {
			return
		}
		if !inbox.ValidSlug(slug) || !inbox.ValidIngestKey(key) || !utf8.ValidString(name) || name == "" {
			t.Fatalf("accepted %q / %q / %q", slug, key, name)
		}
		if pointer != "" && pointer[0] != '/' {
			t.Fatalf("accepted the pointer %q", pointer)
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(cfg), &obj) != nil || obj == nil {
			t.Fatalf("accepted the config %q", cfg)
		}
		if maxBody < 1 || maxBody > inbox.MaxBodyBytesCeiling || days < 0 || maxCount < 0 {
			t.Fatalf("accepted out-of-range limits %d %d %d", maxBody, days, maxCount)
		}
	})
}
