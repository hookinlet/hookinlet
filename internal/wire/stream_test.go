// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func parseStream(t *testing.T, raw string, lastEventID ...string) (StreamRequest, error) {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ParseStream(v, lastEventID)
}

func TestParseStreamStartingPoint(t *testing.T) {
	// No cursor: follow from now, with no replay.
	r, err := parseStream(t, "")
	if err != nil || r.After != nil {
		t.Errorf("no cursor: %+v, %v", r, err)
	}
	// after=0 is a real cursor (replay everything), not "unset".
	if r, err = parseStream(t, "after=0"); err != nil || r.After == nil || *r.After != 0 {
		t.Errorf("after=0: %+v, %v", r, err)
	}
	if r, err = parseStream(t, "after=17"); err != nil || *r.After != 17 {
		t.Errorf("after=17: %+v, %v", r, err)
	}
	// Last-Event-ID beats after: a reconnecting client reuses its URL.
	if r, err = parseStream(t, "after=5", "40"); err != nil || *r.After != 40 {
		t.Errorf("Last-Event-ID should win: %+v, %v", r, err)
	}
	if r, err = parseStream(t, "", "40"); err != nil || *r.After != 40 {
		t.Errorf("Last-Event-ID alone: %+v, %v", r, err)
	}
	// An empty Last-Event-ID carries no information.
	if r, err = parseStream(t, "after=5", ""); err != nil || *r.After != 5 {
		t.Errorf("empty Last-Event-ID: %+v, %v", r, err)
	}
}

func TestParseStreamFilters(t *testing.T) {
	r, err := parseStream(t, "type=a&type=b%2Fc&verified=true")
	if err != nil || len(r.Types) != 2 || r.Types[1] != "b/c" || r.Verified == nil || !*r.Verified {
		t.Errorf("filters: %+v, %v", r, err)
	}
	if r, err = parseStream(t, "verified=false"); err != nil || r.Verified == nil || *r.Verified {
		t.Errorf("verified=false: %+v, %v", r, err)
	}
}

func TestParseStreamRejects(t *testing.T) {
	tests := []struct {
		raw  string
		last []string
		bad  string
	}{
		{"limit=5", nil, "limit"},
		{"since=2026-01-01T00:00:00Z", nil, "since"},
		{"fields=summary", nil, "fields"},
		{"before=5", nil, "before"},
		{"after=1&after=2", nil, "after"},
		{"after=-1", nil, "after"},
		{"after=abc", nil, "after"},
		{"after=", nil, "after"},
		{"type=", nil, "type"},
		{"verified=maybe", nil, "verified"},
		{"verified=1", nil, "verified"},
		{"", []string{"abc"}, "Last-Event-ID"},
		{"", []string{"-5"}, "Last-Event-ID"},
		{"", []string{"1.5"}, "Last-Event-ID"},
		{"", []string{" 5"}, "Last-Event-ID"},
		{"", []string{"5", "6"}, "Last-Event-ID"},
		{"", []string{"99999999999999999999"}, "Last-Event-ID"},
	}
	for _, tt := range tests {
		_, err := parseStream(t, tt.raw, tt.last...)
		var pe *ParamError
		if !errors.As(err, &pe) || pe.Param != tt.bad {
			t.Errorf("%q %v: error = %v, want a ParamError for %q", tt.raw, tt.last, err, tt.bad)
		}
	}
}

func TestEventFormat(t *testing.T) {
	m := stored([]byte(`{"a":1}`))
	m.Seq = 42
	frame, err := Event(FromStored(m, "my-inbox", "none", true))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(frame), "\n")
	if len(lines) != 5 || lines[0] != "id: 42" || lines[1] != "event: message" || !strings.HasPrefix(lines[2], "data: ") || lines[3] != "" || lines[4] != "" {
		t.Fatalf("frame = %q", frame)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[2], "data: ")), &decoded); err != nil {
		t.Fatalf("data is not JSON: %v", err)
	}
	if decoded["seq"] != 42.0 || decoded["inbox"] != "my-inbox" {
		t.Errorf("data = %v", decoded)
	}
	// The data is the same JSON the REST API returns for the message.
	rest, _ := Marshal(FromStored(m, "my-inbox", "none", true))
	if strings.TrimPrefix(lines[2], "data: ") != string(rest) {
		t.Error("the event data differs from the REST representation")
	}
}

// A sender controls what is in the body, so no body may be able to end an event
// early and start another one that looks like it came from the server.
func TestAnEventCannotBeForgedFromABody(t *testing.T) {
	nasty := []string{
		"line one\nid: 999\nevent: message\ndata: {\"seq\":999}\n\n",
		"carriage\rreturn\r\nand more",
		"\n\n",
		"\r\n\r\n",
		"data: x\n\n",
		`{"a":"\n\nid: 999\n\n"}`, // already escaped inside a JSON string
		"a b c",
		"\x00\x01\x02\n\x7f",
	}
	for _, body := range nasty {
		m := stored([]byte(body))
		m.Headers = map[string][]string{"X-Evil": {"a\nb", "c\rd", "id: 1\n\n"}}
		m.Query = "x=\n\nid: 5"
		m.DeliveryID = "d\nid: 7"
		frame, err := Event(FromStored(m, "i", "none", true))
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		// Read the frame the way an SSE client does: split into lines and count
		// events. Exactly one event, whatever the body held.
		var events, ids int
		sc := bufio.NewScanner(bytes.NewReader(frame))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id:"):
				ids++
			case line == "":
				events++
			}
		}
		if ids != 1 || events != 1 {
			t.Errorf("body %q produced %d ids and %d event terminators in %q", body, ids, events, frame)
		}
		if bytes.Count(frame, []byte("\n\n")) != 1 || !bytes.HasSuffix(frame, []byte("\n\n")) {
			t.Errorf("body %q: the frame contains a blank line before its end: %q", body, frame)
		}
		if bytes.ContainsRune(frame[:len(frame)-2], '\r') {
			t.Errorf("body %q: a carriage return survives in %q", body, frame)
		}
	}
}

func TestStreamConstants(t *testing.T) {
	if Retry != "retry: 3000\n\n" || Ping != ": ping\n\n" {
		t.Error("the stream constants changed")
	}
	// Comments and retry are not events: a client must not see them as messages.
	var _ inbox.Message
}
