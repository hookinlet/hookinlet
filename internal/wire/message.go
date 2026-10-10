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

// setBody picks the representation. JSON is embedded as JSON only when the bytes
// are valid UTF-8 as well: encoding/json's validity check does not look at
// UTF-8, and writing invalid bytes into a JSON response would make it invalid
// for strict parsers or silently altered by lenient ones.
func (m *Message) setBody(body []byte) {
	switch {
	case !utf8.Valid(body):
		s := base64.StdEncoding.EncodeToString(body)
		m.BodyBase64 = &s
	case json.Valid(body):
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
