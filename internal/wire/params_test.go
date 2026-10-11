// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

func parse(t *testing.T, raw string) (PageRequest, error) {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("test input %q: %v", raw, err)
	}
	return parseMessages(v)
}

func TestParseMessagesDefaults(t *testing.T) {
	req, err := parse(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Limit != 50 || req.Summary || req.Query.After != nil || req.Query.Before != nil || len(req.Query.Types) != 0 {
		t.Errorf("defaults = %+v", req)
	}
}

func TestParseMessagesValid(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		raw   string
		check func(t *testing.T, r PageRequest)
	}{
		{"after=0", func(t *testing.T, r PageRequest) {
			if r.Query.After == nil || *r.Query.After != 0 {
				t.Error("after=0 must be a real cursor, not 'unset'")
			}
		}},
		{"after=123456", func(t *testing.T, r PageRequest) {
			if *r.Query.After != 123456 {
				t.Error("after")
			}
		}},
		{"before=9", func(t *testing.T, r PageRequest) {
			if *r.Query.Before != 9 {
				t.Error("before")
			}
		}},
		{"after=007", func(t *testing.T, r PageRequest) {
			if *r.Query.After != 7 {
				t.Error("leading zeros are just digits")
			}
		}},
		{"limit=1", func(t *testing.T, r PageRequest) {
			if r.Limit != 1 || r.Query.Limit != 1 {
				t.Error("limit")
			}
		}},
		{"limit=500", func(t *testing.T, r PageRequest) {
			if r.Limit != 500 {
				t.Error("limit 500")
			}
		}},
		{"limit=501", func(t *testing.T, r PageRequest) {
			if r.Limit != 500 {
				t.Errorf("limit above the maximum should be clamped, got %d", r.Limit)
			}
		}},
		{"limit=999999999999", func(t *testing.T, r PageRequest) {
			if r.Limit != 500 {
				t.Errorf("a huge limit should be clamped, got %d", r.Limit)
			}
		}},
		{"type=a", func(t *testing.T, r PageRequest) {
			if !reflect.DeepEqual(r.Query.Types, []string{"a"}) {
				t.Error("type")
			}
		}},
		{"type=a&type=b&type=a.b%2Fc", func(t *testing.T, r PageRequest) {
			if !reflect.DeepEqual(r.Query.Types, []string{"a", "b", "a.b/c"}) {
				t.Errorf("types = %v", r.Query.Types)
			}
		}},
		{"since=2026-03-01T12:00:00Z", func(t *testing.T, r PageRequest) {
			if !r.Query.Since.Equal(t0) {
				t.Error("since")
			}
		}},
		{"since=2026-03-01T05:00:00-07:00", func(t *testing.T, r PageRequest) {
			if !r.Query.Since.Equal(t0) || r.Query.Since.Location() != time.UTC {
				t.Errorf("an offset timestamp should be the same instant in UTC, got %v", r.Query.Since)
			}
		}},
		{"until=2026-03-01T12:00:00.123456789Z", func(t *testing.T, r PageRequest) {
			if r.Query.Until.Nanosecond() != 123456789 {
				t.Error("fractional seconds")
			}
		}},
		{"verified=true", func(t *testing.T, r PageRequest) {
			if r.Query.Verified == nil || !*r.Query.Verified {
				t.Error("verified=true")
			}
		}},
		{"verified=false", func(t *testing.T, r PageRequest) {
			if r.Query.Verified == nil || *r.Query.Verified {
				t.Error("verified=false")
			}
		}},
		{"delivery_id=evt_1%2Fx", func(t *testing.T, r PageRequest) {
			if r.Query.DeliveryID != "evt_1/x" {
				t.Error("delivery_id")
			}
		}},
		{"fields=summary", func(t *testing.T, r PageRequest) {
			if !r.Summary {
				t.Error("summary")
			}
		}},
		{"fields=full", func(t *testing.T, r PageRequest) {
			if r.Summary {
				t.Error("full")
			}
		}},
		{"fields=", func(t *testing.T, r PageRequest) {
			if r.Summary {
				t.Error("empty fields means full")
			}
		}},
		{"since=2026-03-01T00:00:00Z&until=2026-03-02T00:00:00Z", func(t *testing.T, r PageRequest) {
			if r.Query.Since.IsZero() || r.Query.Until.IsZero() {
				t.Error("window")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			req, err := parse(t, tt.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.check(t, req)
		})
	}
}

// Following a next link must ask for the same thing, one page further on.
func TestNextLinkRoundTrips(t *testing.T) {
	msgs := func(seqs ...int64) []*inbox.Message {
		var out []*inbox.Message
		for _, s := range seqs {
			out = append(out, &inbox.Message{Seq: s})
		}
		return out
	}
	filters := "&type=a&type=b%2Fc&since=2026-03-01T12:00:00Z&until=2026-03-02T00:00:00.5Z&verified=true&delivery_id=evt%201&fields=summary&limit=7"

	t.Run("ascending carries the last seq and every filter", func(t *testing.T) {
		req, _ := parse(t, "after=10"+filters)
		link := req.NextLink("my-inbox", msgs(11, 12, 13))
		next := mustParseLink(t, link, "my-inbox")
		want := req
		want.Query.After = ptr(int64(13))
		if !reflect.DeepEqual(next, want) {
			t.Errorf("next = %+v\nwant   %+v\nlink %s", next, want, link)
		}
	})
	t.Run("ascending with no messages repeats the cursor", func(t *testing.T) {
		req, _ := parse(t, "after=500&limit=20")
		link := req.NextLink("my-inbox", nil)
		next := mustParseLink(t, link, "my-inbox")
		if next.Query.After == nil || *next.Query.After != 500 || next.Limit != 20 {
			t.Errorf("link %s -> %+v", link, next)
		}
	})
	t.Run("descending moves before to the oldest returned", func(t *testing.T) {
		req, _ := parse(t, "before=100"+filters)
		link := req.NextLink("my-inbox", msgs(99, 98, 97))
		next := mustParseLink(t, link, "my-inbox")
		want := req
		want.Query.Before = ptr(int64(97))
		if !reflect.DeepEqual(next, want) {
			t.Errorf("next = %+v\nwant   %+v", next, want)
		}
	})
	t.Run("newest-first with no cursor becomes before", func(t *testing.T) {
		req, _ := parse(t, "")
		next := mustParseLink(t, req.NextLink("my-inbox", msgs(50, 49)), "my-inbox")
		if next.Query.Before == nil || *next.Query.Before != 49 || next.Query.After != nil {
			t.Errorf("next = %+v", next)
		}
	})
	t.Run("descending stops after seq 1 and on an empty page", func(t *testing.T) {
		req, _ := parse(t, "before=5")
		if link := req.NextLink("i", msgs(4, 3, 2, 1)); link != "" {
			t.Errorf("link after reaching seq 1: %s", link)
		}
		if link := req.NextLink("i", nil); link != "" {
			t.Errorf("link after an empty page: %s", link)
		}
	})
	t.Run("the slug is escaped", func(t *testing.T) {
		req, _ := parse(t, "after=0")
		if link := req.NextLink("a-b", msgs(1)); !strings.HasPrefix(link, "/v1/inboxes/a-b/messages?") {
			t.Errorf("link = %s", link)
		}
		if link := req.NextLink("x/../y", msgs(1)); strings.Contains(link, "/../") {
			t.Errorf("an unescaped slug in the link: %s", link)
		}
	})
}

func mustParseLink(t *testing.T, link, slug string) PageRequest {
	t.Helper()
	prefix := "/v1/inboxes/" + slug + "/messages?"
	if !strings.HasPrefix(link, prefix) {
		t.Fatalf("link %q does not start with %q", link, prefix)
	}
	req, err := parse(t, strings.TrimPrefix(link, prefix))
	if err != nil {
		t.Fatalf("link %q does not parse: %v", link, err)
	}
	return req
}

func ptr[T any](v T) *T { return &v }
