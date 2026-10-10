// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// maxParamLen bounds the length of any single string parameter.
const maxParamLen = 256

// ParamError reports a query parameter the caller got wrong. Its text is safe to
// show to the caller.
type ParamError struct {
	Param  string
	Reason string
}

func (e *ParamError) Error() string {
	if e.Param == "" {
		return e.Reason
	}
	return "parameter \"" + e.Param + "\": " + e.Reason
}

func bad(param, reason string) error { return &ParamError{Param: param, Reason: reason} }

// PageRequest is a parsed request for a page of messages.
type PageRequest struct {
	// Query is ready to hand to the Store, except for BodyLimit, which the page
	// assembly sets.
	Query inbox.Query
	// Limit is the effective page size, after the default and the maximum.
	Limit int
	// Summary asks for messages without bodies.
	Summary bool
}

var knownParams = map[string]bool{
	"after": true, "before": true, "limit": true, "type": true, "since": true,
	"until": true, "verified": true, "delivery_id": true, "fields": true,
}

// ParseMessages reads the query parameters of a request for a page of messages.
//
// Parsing is strict. An unknown parameter, a repeated one (except type), or a
// malformed value is an error rather than being ignored: a misspelled "limt"
// that silently returned the default page size would look like it worked.
// A limit above the maximum is the one exception; it is clamped.
func ParseMessages(v url.Values) (PageRequest, error) {
	for k := range v {
		if !knownParams[k] {
			return PageRequest{}, bad(truncate(k, 64), "unknown parameter")
		}
	}
	for k, vs := range v {
		if k != "type" && len(vs) > 1 {
			return PageRequest{}, bad(k, "may be given only once")
		}
	}

	var req PageRequest
	q := &req.Query
	var err error

	if s, ok := v["after"]; ok {
		n, err := parseSeq("after", s[0])
		if err != nil {
			return req, err
		}
		q.After = &n
	}
	if s, ok := v["before"]; ok {
		n, err := parseSeq("before", s[0])
		if err != nil {
			return req, err
		}
		q.Before = &n
	}

	req.Limit = inbox.DefaultQueryLimit
	if s, ok := v["limit"]; ok {
		n, err := parseDigits("limit", s[0], 1)
		if err != nil {
			return req, err
		}
		req.Limit = int(min(n, int64(inbox.MaxQueryLimit)))
	}
	q.Limit = req.Limit

	if types := v["type"]; len(types) > 0 {
		if len(types) > inbox.MaxQueryTypes {
			return req, bad("type", "too many values")
		}
		for _, t := range types {
			if t == "" || len(t) > maxParamLen || !utf8.ValidString(t) {
				return req, bad("type", "each value must be 1 to 256 characters of valid UTF-8")
			}
		}
		q.Types = types
	}

	if s, ok := v["since"]; ok {
		if q.Since, err = parseTime("since", s[0]); err != nil {
			return req, err
		}
	}
	if s, ok := v["until"]; ok {
		if q.Until, err = parseTime("until", s[0]); err != nil {
			return req, err
		}
	}

	if s, ok := v["verified"]; ok {
		switch s[0] {
		case "true":
			t := true
			q.Verified = &t
		case "false":
			f := false
			q.Verified = &f
		default:
			return req, bad("verified", "must be true or false")
		}
	}

	if s, ok := v["delivery_id"]; ok {
		if s[0] == "" || len(s[0]) > maxParamLen || !utf8.ValidString(s[0]) {
			return req, bad("delivery_id", "must be 1 to 256 characters of valid UTF-8")
		}
		q.DeliveryID = s[0]
	}

	if s, ok := v["fields"]; ok {
		switch s[0] {
		case "summary":
			req.Summary = true
		case "full", "":
		default:
			return req, bad("fields", "must be summary or full")
		}
	}

	if err := q.Validate(); err != nil {
		return req, bad("", strings.TrimPrefix(err.Error(), inbox.ErrInvalid.Error()+": "))
	}
	return req, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseDigits parses a decimal integer written with digits only, at least min.
// Signs, spaces, and anything else are rejected, as is a value too large to be
// a sequence number.
func parseDigits(param, s string, min int64) (int64, error) {
	if s == "" || len(s) > 18 {
		return 0, bad(param, "must be a whole number")
	}
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, bad(param, "must be a whole number")
		}
		n = n*10 + int64(s[i]-'0')
	}
	if n < min {
		return 0, bad(param, "must be at least "+formatInt(min))
	}
	return n, nil
}

func parseSeq(param, s string) (int64, error) { return parseDigits(param, s, 0) }

func formatInt(n int64) string { return strconv.FormatInt(n, 10) }

func parseTime(param, s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, bad(param, "must be an RFC 3339 timestamp such as 2026-03-01T12:00:00Z")
	}
	return t.UTC(), nil
}

// NextLink returns the relative URL of the page that follows msgs, or "" if there
// is none.
//
// Polling forward (after=) always has a next link, even for an empty page: it is
// the same cursor again, so an agent can loop on it. Going backward ends when
// the oldest message has been returned.
func (r PageRequest) NextLink(slug string, msgs []*inbox.Message) string {
	v := url.Values{}
	switch {
	case r.Query.Ascending():
		cursor := *r.Query.After
		if n := len(msgs); n > 0 {
			cursor = msgs[n-1].Seq
		}
		v.Set("after", formatInt(cursor))
	default:
		n := len(msgs)
		if n == 0 || msgs[n-1].Seq <= 1 {
			return ""
		}
		v.Set("before", formatInt(msgs[n-1].Seq))
	}
	v.Set("limit", formatInt(int64(r.Limit)))
	for _, t := range r.Query.Types {
		v.Add("type", t)
	}
	if !r.Query.Since.IsZero() {
		v.Set("since", r.Query.Since.UTC().Format(time.RFC3339Nano))
	}
	if !r.Query.Until.IsZero() {
		v.Set("until", r.Query.Until.UTC().Format(time.RFC3339Nano))
	}
	if r.Query.Verified != nil {
		if *r.Query.Verified {
			v.Set("verified", "true")
		} else {
			v.Set("verified", "false")
		}
	}
	if r.Query.DeliveryID != "" {
		v.Set("delivery_id", r.Query.DeliveryID)
	}
	if r.Summary {
		v.Set("fields", "summary")
	}
	return "/v1/inboxes/" + url.PathEscape(slug) + "/messages?" + v.Encode()
}
