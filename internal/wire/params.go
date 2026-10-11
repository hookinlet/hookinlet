// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"net/url"
	"strconv"
	"strings"
	"time"

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
	// Budget caps the encoded size of one page. Zero means PageBudget. A
	// stream uses a smaller one, so replaying a large backlog never builds a
	// huge page in memory.
	Budget int64
	// BodyCap, when positive, leaves out the body of any message larger than
	// this many bytes and marks it body_truncated, instead of loading it. The
	// REST API leaves it zero. MCP tools set it to protect an agent's context.
	BodyCap int64
}

// MessagesParams are the query parameters of a request for a page of messages,
// as the generated code read them from a query string that the document has
// already approved. Nil means the caller did not give the parameter.
type MessagesParams struct {
	After, Before, Limit *int
	Types                []string
	Since, Until         *time.Time
	Verified             *bool
	DeliveryID           *string
	Summary              bool // fields=summary
}

// NewPageRequest builds the request for a page. A limit above the maximum is
// clamped, not refused. What the document cannot say is checked here: the rules
// that tie the parameters to one another (inbox.Query.Validate).
func NewPageRequest(p MessagesParams) (PageRequest, error) {
	var req PageRequest
	q := &req.Query
	if p.After != nil {
		n := int64(*p.After)
		q.After = &n
	}
	if p.Before != nil {
		n := int64(*p.Before)
		q.Before = &n
	}
	req.Limit = inbox.DefaultQueryLimit
	if p.Limit != nil {
		req.Limit = int(min(int64(*p.Limit), int64(inbox.MaxQueryLimit)))
	}
	q.Limit = req.Limit
	q.Types = p.Types
	if p.Since != nil {
		q.Since = p.Since.UTC()
	}
	if p.Until != nil {
		q.Until = p.Until.UTC()
	}
	q.Verified = p.Verified
	if p.DeliveryID != nil {
		q.DeliveryID = *p.DeliveryID
	}
	req.Summary = p.Summary
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
