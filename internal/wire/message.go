// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package wire defines how messages and pages are written to and read from the
// outside world: the JSON form of a message, the meaning of the query
// parameters, and the paging links. The REST API uses it, and the MCP tools use
// the same code, so there is one data path and one set of rules.
package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"unicode/utf8"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// TimeLayout formats timestamps with a fixed width, so they sort as text.
const TimeLayout = "2006-01-02T15:04:05.000000Z"

// Message is the JSON form of a stored message.
//
// Exactly one of Body, BodyText, and BodyBase64 is present, or none when bodies
// are left out:
//   - Body holds the body itself when it is valid UTF-8 and valid JSON;
//   - BodyText holds it when it is valid UTF-8 but not JSON;
//   - BodyBase64 holds it otherwise.
type Message struct {
	Seq         int64       `json:"seq"`
	Inbox       string      `json:"inbox"`
	ReceivedAt  string      `json:"received_at"`
	Method      string      `json:"method"`
	Path        string      `json:"path"`
	Query       string      `json:"query"`
	Headers     http.Header `json:"headers"`
	ContentType string      `json:"content_type"`
	Size        int64       `json:"size"`
	Verified    bool        `json:"verified"`
	Verifier    string      `json:"verifier"`
	VerifyError *string     `json:"verify_error"`
	DeliveryID  *string     `json:"delivery_id"`
	EventType   *string     `json:"event_type"`
	SourceIP    *string     `json:"source_ip"`

	// BodyTruncated is set, only when a size cap applies, on a message whose body
	// was left out for being larger than the cap. Size gives its true length.
	BodyTruncated bool `json:"body_truncated,omitempty"`

	Body       json.RawMessage `json:"body,omitempty"`
	BodyText   *string         `json:"body_text,omitempty"`
	BodyBase64 *string         `json:"body_base64,omitempty"`
}

// FromStored converts a stored message. slug and verifier name the inbox it
// belongs to. If withBody is false, or the stored message has no body loaded,
// none of the body fields are set.
func FromStored(m *inbox.Message, slug, verifier string, withBody bool) Message {
	out := Message{
		Seq:         m.Seq,
		Inbox:       slug,
		ReceivedAt:  m.ReceivedAt.UTC().Format(TimeLayout),
		Method:      m.Method,
		Path:        m.PathSuffix,
		Query:       m.Query,
		Headers:     m.Headers,
		ContentType: m.ContentType,
		Size:        m.Size,
		Verified:    m.Verified,
		Verifier:    verifier,
		VerifyError: optional(m.VerifyError),
		DeliveryID:  optional(m.DeliveryID),
		EventType:   optional(m.EventType),
		SourceIP:    optional(m.SourceIP),
	}
	if out.Headers == nil {
		out.Headers = http.Header{}
	}
	if withBody && m.Body != nil {
		out.setBody(m.Body)
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// MaxEmbeddedJSONDepth is the deepest JSON nesting a body may have and still be
// embedded as JSON. encoding/json accepts 10,000 levels, but many clients do not
// (Python's json module raises RecursionError at roughly 1,000), and a single
// body that a client cannot parse would make every page containing it
// unreadable, leaving an agent polling with after= stuck on it for good. Deeper
// bodies are delivered as text.
const MaxEmbeddedJSONDepth = 64

// setBody picks the representation. JSON is embedded as JSON only when the bytes
// are valid UTF-8 as well (encoding/json's validity check does not look at
// UTF-8, and invalid bytes would make the response invalid for strict parsers or
// silently altered by lenient ones) and are not nested too deeply for clients to
// parse.
func (m *Message) setBody(body []byte) {
	switch {
	case !utf8.Valid(body):
		s := base64.StdEncoding.EncodeToString(body)
		m.BodyBase64 = &s
	case json.Valid(body) && jsonDepth(body) <= MaxEmbeddedJSONDepth:
		m.Body = json.RawMessage(bytes.Clone(body))
	default:
		s := string(body)
		m.BodyText = &s
	}
}

// Marshal encodes v as JSON the way every response is written: compact, with
// HTML characters left alone, and no trailing newline. Leaving "<" and "&" as
// they are keeps a sender's JSON body byte-faithful instead of rewriting it
// into an equivalent but different text.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// jsonDepth returns the deepest nesting of arrays and objects in a document that
// is already known to be valid JSON. Brackets inside strings do not count.
func jsonDepth(b []byte) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			if depth++; depth > deepest {
				deepest = depth
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return deepest
}

// messageOverhead covers the field names, punctuation, and fixed-width values of
// one encoded message.
const messageOverhead = 700

// encodedSize estimates how many bytes the message takes in a response. The
// estimate is never short, so a budget built on it really does bound the
// response: control characters and invalid UTF-8, which JSON writes as six-byte
// \uXXXX escapes, are counted at their escaped size.
func (m Message) encodedSize() int64 {
	n := int64(messageOverhead)
	for _, s := range []string{m.Inbox, m.Method, m.Path, m.Query, m.ContentType, m.Verifier} {
		n += jsonStringLen(s)
	}
	for k, vs := range m.Headers {
		n += jsonStringLen(k) + 4
		for _, v := range vs {
			n += jsonStringLen(v) + 1
		}
	}
	for _, p := range []*string{m.VerifyError, m.DeliveryID, m.EventType, m.SourceIP, m.BodyText, m.BodyBase64} {
		if p != nil {
			n += jsonStringLen(*p)
		}
	}
	return n + int64(len(m.Body))
}

// jsonStringLen is the size of s as a JSON string, quotes included, counting
// every control character at its six-byte escaped size and every invalid byte
// as the six-byte \ufffd the encoder substitutes for it.
func jsonStringLen(s string) int64 {
	n := int64(2)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c < utf8.RuneSelf:
			switch c {
			case '"', '\\', '\n', '\r', '\t':
				n += 2
			default:
				if c < 0x20 {
					n += 6
				} else {
					n++
				}
			}
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				n += 6
			case r == '\u2028' || r == '\u2029':
				n += 6
			default:
				n += int64(size)
			}
			i += size
		}
	}
	return n
}
