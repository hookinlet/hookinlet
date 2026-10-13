// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// The fuzz targets here run their seeds in a normal `go test`. To fuzz one:
//
//	go test -run='^$' -fuzz=FuzzMessageBody -fuzztime=90s ./internal/wire

// FuzzMessageBody: whatever a sender posts must come back byte for byte, in
// exactly one representation, in JSON every parser accepts, and in an SSE frame
// that cannot be split.
func FuzzMessageBody(f *testing.F) {
	f.Add([]byte(`{"type":"a","n":1.5e3,"s":"<&>"}`), "x", "text/plain", "k", "v")
	f.Add([]byte(""), "", "", "", "")
	f.Add([]byte("\xff\xfe\x00"), "a\nb", "t\r\n", "K\u0085", "v ")
	f.Add([]byte(`"\ud800"`), "p", "c", "h", "\xc2\x85")
	f.Add([]byte(`[1e400]`), "p", "c", "h", "v")
	f.Add([]byte(`{"a":"é😀"}`), "p", "c", "h", "v")
	f.Add([]byte(strings.Repeat("[", 70)+strings.Repeat("]", 70)), "p", "c", "h", "v")
	f.Add([]byte("1"+strings.Repeat("0", 150)), "p", "c", "h", "v")
	f.Fuzz(func(t *testing.T, body []byte, path, ctype, hk, hv string) {
		m := &inbox.Message{
			Seq: 3, ReceivedAt: time.Unix(1_800_000_000, 5000).UTC(), Method: "POST", PathSuffix: path, Query: path,
			Headers: http.Header{hk: {hv}}, ContentType: ctype, Size: int64(len(body)), Body: body,
		}
		wm := FromStored(m, "slug", "none", true)
		n := 0
		for _, set := range []bool{wm.Body != nil, wm.BodyText != nil, wm.BodyBase64 != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d body representations for %q", n, body)
		}
		raw, err := Marshal(wm)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(raw)) > wm.encodedSize() {
			t.Fatalf("encodedSize %d is short of the %d bytes written", wm.encodedSize(), len(raw))
		}
		if !json.Valid(raw) || !utf8.Valid(raw) {
			t.Fatalf("response is not valid UTF-8 JSON: %q", raw)
		}
		switch {
		case wm.Body != nil:
			if !bytes.Equal(wm.Body, body) {
				t.Fatal("embedded JSON differs from the body")
			}
			var v any
			if err := json.Unmarshal(wm.Body, &v); err != nil {
				t.Fatalf("a body embedded as JSON does not decode: %v", err)
			}
			if jsonDepth(body) > MaxEmbeddedJSONDepth {
				t.Fatal("too deep to embed")
			}
		case wm.BodyText != nil:
			if *wm.BodyText != string(body) || !utf8.ValidString(*wm.BodyText) {
				t.Fatal("body_text differs from the body")
			}
		default:
			got, err := base64.StdEncoding.DecodeString(*wm.BodyBase64)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatal("body_base64 differs from the body")
			}
		}
		// The envelope round-trips through a strict reader.
		var back Message
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("response does not decode: %v", err)
		}
		ev, err := Event(wm)
		if err != nil {
			t.Fatalf("a message could not be framed: %v", err)
		}
		frame := string(ev)
		if !strings.HasSuffix(frame, "\n\n") || strings.Count(frame, "\n") != 4 || strings.ContainsAny(frame, "\r\u0085  ") {
			t.Fatalf("malformed SSE frame: %q", frame)
		}
		lines := strings.Split(strings.TrimSuffix(frame, "\n\n"), "\n")
		if len(lines) != 3 || lines[0] != "id: 3" || lines[1] != "event: message" || !strings.HasPrefix(lines[2], "data: ") {
			t.Fatalf("frame lines: %q", lines)
		}
		if !json.Valid([]byte(strings.TrimPrefix(lines[2], "data: "))) {
			t.Fatal("frame data is not JSON")
		}
	})
}

var allDigits = regexp.MustCompile(`^[0-9]{1,18}$`)

// FuzzStreamRequest: a Last-Event-ID is a plain decimal sequence number or it
// is refused, and the result never goes negative.
func FuzzStreamRequest(f *testing.F) {
	for _, s := range []string{"", "0", "12", "-1", "+1", " 1", "1 ", "01", "1e3", "999999999999999999", "9999999999999999999", "１２", "0x10", "1\n", "\x00"} {
		f.Add(s, 0, false)
	}
	f.Fuzz(func(t *testing.T, lastID string, after int, hasAfter bool) {
		p := StreamParams{LastEventID: &lastID}
		if hasAfter {
			p.After = &after
		}
		req, err := NewStreamRequest(p)
		if lastID != "" {
			if (err == nil) != allDigits.MatchString(lastID) {
				t.Fatalf("Last-Event-ID %q: err = %v", lastID, err)
			}
			if err == nil {
				want, _ := strconv.ParseInt(lastID, 10, 64)
				if req.After == nil || *req.After != want {
					t.Fatalf("Last-Event-ID %q parsed as %v", lastID, req.After)
				}
			}
		}
		if err != nil {
			var pe *ParamError
			if !asParamError(err, &pe) {
				t.Fatalf("error is not a ParamError: %v", err)
			}
		}
	})
}

func asParamError(err error, target **ParamError) bool {
	pe, ok := err.(*ParamError) //nolint:errorlint // the error is returned unwrapped
	if ok {
		*target = pe
	}
	return ok
}

// FuzzPageRequest: parameters that pass validation produce a bounded query and
// a next link that is a well-formed relative URL.
func FuzzPageRequest(f *testing.F) {
	f.Add(int64(0), int64(-1), int64(50), "a.b", "2026-03-01T12:00:00Z", "d-1", "ep", 1, 2)
	f.Add(int64(-1), int64(5), int64(0), "", "", "", "", 0, 0)
	f.Add(int64(1<<62), int64(0), int64(1<<40), "x&y=z#", "x", "\n", "%", 5, 1)
	f.Fuzz(func(t *testing.T, after, before, limit int64, typ, since, delivery, epoch string, mode, verified int) {
		p := MessagesParams{Types: []string{typ}, Epoch: &epoch, DeliveryID: &delivery, Summary: mode&1 == 1}
		if mode&2 != 0 {
			a := int(after)
			p.After = &a
		}
		if mode&4 != 0 {
			b := int(before)
			p.Before = &b
		}
		if mode&8 != 0 {
			l := int(limit)
			p.Limit = &l
		}
		if tm, err := time.Parse(time.RFC3339Nano, since); err == nil {
			p.Since = &tm
		}
		if verified&1 == 1 {
			v := verified&2 == 2
			p.Verified = &v
		}
		req, err := NewPageRequest(p)
		if err != nil {
			return
		}
		if req.Limit < 0 || req.Limit > inbox.MaxQueryLimit || req.Query.Limit != req.Limit {
			t.Fatalf("limit %d escapes its bounds", req.Limit)
		}
		if req.Query.Validate() != nil {
			t.Fatal("NewPageRequest returned a query that does not validate")
		}
		msgs := []*inbox.Message{{Seq: 5}, {Seq: 4}}
		for _, ms := range [][]*inbox.Message{nil, msgs} {
			link := req.NextLink("my-slug", epoch, ms)
			if link == "" {
				continue
			}
			u, err := url.Parse(link)
			if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/v1/inboxes/my-slug/messages") {
				t.Fatalf("bad next link %q (%v)", link, err)
			}
			if _, err := url.ParseQuery(u.RawQuery); err != nil {
				t.Fatalf("bad next link query %q", link)
			}
			if got := u.Query().Get("delivery_id"); got != delivery && delivery != "" {
				t.Fatalf("delivery_id %q lost in %q", delivery, link)
			}
		}
	})
}
