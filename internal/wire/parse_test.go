// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"net/url"
	"strconv"
	"time"
)

// These read a query string into the typed parameters the generated code would
// hand over, for tests that are written as query strings. They take well-formed
// input only: what a query string may hold is the document's to say, and is
// tested through the server.

func parseMessages(v url.Values) (PageRequest, error) {
	var p MessagesParams
	num := func(k string) *int {
		if s, ok := v[k]; ok {
			n, _ := strconv.Atoi(s[0])
			return &n
		}
		return nil
	}
	tm := func(k string) *time.Time {
		if s, ok := v[k]; ok {
			t, _ := time.Parse(time.RFC3339Nano, s[0])
			return &t
		}
		return nil
	}
	p.After, p.Before, p.Limit = num("after"), num("before"), num("limit")
	p.Types = v["type"]
	p.Since, p.Until = tm("since"), tm("until")
	if s, ok := v["verified"]; ok {
		b := s[0] == "true"
		p.Verified = &b
	}
	if s, ok := v["delivery_id"]; ok {
		p.DeliveryID = &s[0]
	}
	p.Summary = v.Get("fields") == "summary"
	return NewPageRequest(p)
}

func parseStreamValues(v url.Values, lastEventID []string) (StreamRequest, error) {
	if len(lastEventID) > 1 {
		// The generated code refuses a header given twice before the server gets here.
		return StreamRequest{}, bad("Last-Event-ID", "may be given only once")
	}
	p := StreamParams{Types: v["type"]}
	if len(lastEventID) == 1 {
		p.LastEventID = &lastEventID[0]
	}
	if s, ok := v["after"]; ok {
		n, _ := strconv.Atoi(s[0])
		p.After = &n
	}
	if s, ok := v["verified"]; ok {
		b := s[0] == "true"
		p.Verified = &b
	}
	return NewStreamRequest(p)
}
