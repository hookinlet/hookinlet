// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package wire

import (
	"bytes"
	"strconv"
)

// StreamRequest is a parsed request to follow an inbox.
type StreamRequest struct {
	// After is where to start, if the request said: from Last-Event-ID (a
	// reconnect) or the after parameter. Nil means "from now", with no replay.
	After *int64
	// Query carries the filters (event types, verified).
	Types    []string
	Verified *bool
}

// StreamParams are the query parameters of a stream request, as the generated
// code read them from a query string that the document has already approved.
type StreamParams struct {
	After    *int
	Types    []string
	Verified *bool
	// LastEventID is the Last-Event-ID header, if the request had one.
	LastEventID *string
}

// NewStreamRequest builds the request for a stream from its parameters.
//
// Last-Event-ID wins over the after parameter. A browser reconnects to the same
// URL it first used, after parameter included, and sends Last-Event-ID to say how
// far it got; the header is the more recent fact.
func NewStreamRequest(p StreamParams) (StreamRequest, error) {
	var req StreamRequest
	if p.After != nil {
		n := int64(*p.After)
		req.After = &n
	}
	if p.LastEventID != nil && *p.LastEventID != "" { // an empty Last-Event-ID means "none yet"
		n, err := parseSeq("Last-Event-ID", *p.LastEventID)
		if err != nil {
			return req, err
		}
		req.After = &n
	}
	req.Types = p.Types
	req.Verified = p.Verified
	return req, nil
}

// Event is one Server-Sent Events frame for a message.
//
//	id: 1234
//	event: message
//	data: {...the same JSON as the REST API...}
//
// The data is on a single line: compact JSON never contains a raw newline, since
// line breaks inside strings are escaped. Frame checks that anyway, because a
// newline in the data would let a sender end the frame and forge another.
func Event(m Message) ([]byte, error) {
	data, err := Marshal(m)
	if err != nil {
		return nil, err
	}
	// U+0085 (NEL) is not a line break in SSE, but many programs split text on it
	// (Python's str.splitlines does), which would cut the data line in two. Inside
	// a JSON string it can be written as an escape. A C2 byte is always the first
	// byte of its character, so this cannot touch any other one.
	data = bytes.ReplaceAll(data, []byte("\xc2\x85"), []byte(`\u0085`))
	if bytes.ContainsAny(data, "\r\n") {
		return nil, errNewlineInData
	}
	var b bytes.Buffer
	b.Grow(len(data) + 48)
	b.WriteString("id: ")
	b.WriteString(strconv.FormatInt(m.Seq, 10))
	b.WriteString("\nevent: message\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes(), nil
}

type streamError string

func (e streamError) Error() string { return string(e) }

const errNewlineInData = streamError("wire: encoded message contains a line break and cannot be sent as an event")

// Retry is the reconnection delay announced once at the start of a stream.
const Retry = "retry: 3000\n\n"

// Ping is the comment sent to keep an idle stream alive. Clients ignore it; it
// keeps proxies from closing the connection and lets the server notice a client
// that has gone.
const Ping = ": ping\n\n"
