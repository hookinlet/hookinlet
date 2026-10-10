// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package verify

import (
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is still a sound MAC, and some senders only offer it
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// hmacConfig is the verifier_config of a generic HMAC inbox.
type hmacConfig struct {
	// Header names the request header carrying the signature. Required.
	Header string `json:"header"`
	// Algorithm is sha1, sha256 (default), sha384, or sha512.
	Algorithm string `json:"algorithm"`
	// Encoding is hex (default), base64, or base64url.
	Encoding string `json:"encoding"`
	// Prefix is literal text before the encoded signature, such as "sha256=".
	Prefix string `json:"prefix"`
	// TimestampHeader names a header holding a Unix timestamp in seconds. When
	// set, the timestamp must be within ToleranceSeconds of the server clock.
	TimestampHeader string `json:"timestamp_header"`
	// PayloadTemplate describes the signed bytes. It must contain {body} once
	// and may contain {timestamp}. Default: "{body}", or "{timestamp}.{body}"
	// when TimestampHeader is set.
	PayloadTemplate string `json:"payload_template"`
	// ToleranceSeconds applies only with TimestampHeader. Default 300.
	ToleranceSeconds int `json:"tolerance_seconds"`
	// DeliveryIDHeader names a header holding a unique delivery id, used to
	// deduplicate retries. It is only as trustworthy as the sender makes it:
	// unless the payload template covers it, it is not signed.
	DeliveryIDHeader string `json:"delivery_id_header"`
}

var hmacAlgorithms = map[string]func() hash.Hash{
	"sha1":   sha1.New,
	"sha256": sha256.New,
	"sha384": sha512.New384,
	"sha512": sha512.New,
}

type segmentKind int

const (
	segLiteral segmentKind = iota
	segTimestamp
	segBody
)

type segment struct {
	kind segmentKind
	lit  string
}

type genericHMAC struct {
	header         string
	newHash        func() hash.Hash
	decode         func(string) ([]byte, error)
	prefix         string
	timestampHdr   string
	segments       []segment
	tolerance      time.Duration
	deliveryIDHdr  string
	hasTimestampIn bool
}

func newHMAC(config json.RawMessage) (Verifier, error) {
	var c hmacConfig
	if err := decodeConfig(config, &c); err != nil {
		return nil, err
	}
	if !validHeaderName(c.Header) {
		return nil, configErr(`"header" is required and must be a valid HTTP header name`)
	}
	v := &genericHMAC{header: textproto.CanonicalMIMEHeaderKey(c.Header), prefix: c.Prefix}

	alg := c.Algorithm
	if alg == "" {
		alg = "sha256"
	}
	newHash, ok := hmacAlgorithms[alg]
	if !ok {
		return nil, configErr(`"algorithm" must be sha1, sha256, sha384, or sha512`)
	}
	v.newHash = newHash

	switch c.Encoding {
	case "", "hex":
		v.decode = hex.DecodeString
	case "base64":
		v.decode = func(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")) }
	case "base64url":
		v.decode = func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")) }
	default:
		return nil, configErr(`"encoding" must be hex, base64, or base64url`)
	}

	if c.TimestampHeader != "" {
		if !validHeaderName(c.TimestampHeader) {
			return nil, configErr(`"timestamp_header" must be a valid HTTP header name`)
		}
		v.timestampHdr = textproto.CanonicalMIMEHeaderKey(c.TimestampHeader)
	}
	if c.ToleranceSeconds != 0 && v.timestampHdr == "" {
		return nil, configErr(`"tolerance_seconds" only applies with "timestamp_header"`)
	}
	var err error
	if v.tolerance, err = toleranceFrom(c.ToleranceSeconds); err != nil {
		return nil, err
	}

	tmpl := c.PayloadTemplate
	if tmpl == "" {
		tmpl = "{body}"
		if v.timestampHdr != "" {
			tmpl = "{timestamp}.{body}"
		}
	}
	if v.segments, err = parseTemplate(tmpl); err != nil {
		return nil, err
	}
	for _, s := range v.segments {
		if s.kind == segTimestamp {
			v.hasTimestampIn = true
		}
	}
	if v.hasTimestampIn && v.timestampHdr == "" {
		return nil, configErr(`"payload_template" uses {timestamp}, which needs "timestamp_header"`)
	}

	if c.DeliveryIDHeader != "" {
		if !validHeaderName(c.DeliveryIDHeader) {
			return nil, configErr(`"delivery_id_header" must be a valid HTTP header name`)
		}
		v.deliveryIDHdr = textproto.CanonicalMIMEHeaderKey(c.DeliveryIDHeader)
	}
	return v, nil
}

// parseTemplate splits a payload template into literal text and the two
// placeholders. It is parsed once, so the delivery's own content can never be
// mistaken for a placeholder. Braces are reserved for the placeholders.
func parseTemplate(t string) ([]segment, error) {
	var segs []segment
	bodies := 0
	for len(t) > 0 {
		i := strings.IndexAny(t, "{}")
		if i < 0 {
			segs = append(segs, segment{kind: segLiteral, lit: t})
			break
		}
		if i > 0 {
			segs = append(segs, segment{kind: segLiteral, lit: t[:i]})
		}
		t = t[i:]
		switch {
		case strings.HasPrefix(t, "{body}"):
			segs, t = append(segs, segment{kind: segBody}), t[len("{body}"):]
			bodies++
		case strings.HasPrefix(t, "{timestamp}"):
			segs, t = append(segs, segment{kind: segTimestamp}), t[len("{timestamp}"):]
		default:
			return nil, configErr(`"payload_template" may only use the placeholders {body} and {timestamp}; braces are reserved`)
		}
	}
	if bodies != 1 {
		return nil, configErr(`"payload_template" must contain {body} exactly once`)
	}
	return segs, nil
}

func (g *genericHMAC) Verify(r *http.Request, body, secret []byte, now time.Time) (Result, error) {
	if err := requireSecret(secret); err != nil {
		return Result{}, err
	}
	header, err := singleHeader(r, g.header)
	if err != nil {
		return Result{}, err
	}
	encoded, ok := strings.CutPrefix(header, g.prefix)
	if !ok {
		return Result{}, fail("malformed signature header")
	}
	sig, err := g.decode(encoded)
	if err != nil || len(sig) == 0 {
		return Result{}, fail("malformed signature header")
	}

	var ts time.Time
	var tsStr string
	if g.timestampHdr != "" {
		if tsStr, err = singleHeader(r, g.timestampHdr); err != nil {
			return Result{}, err
		}
		if ts, ok = parseUnixSeconds(tsStr); !ok {
			return Result{}, fail("malformed timestamp header")
		}
	}

	parts := make([][]byte, 0, len(g.segments))
	for _, s := range g.segments {
		switch s.kind {
		case segLiteral:
			parts = append(parts, []byte(s.lit))
		case segTimestamp:
			parts = append(parts, []byte(tsStr))
		case segBody:
			parts = append(parts, body)
		}
	}
	if !anyEqual(mac(g.newHash, secret, parts...), [][]byte{sig}) {
		return Result{}, fail("signature mismatch")
	}
	if g.timestampHdr != "" && !withinTolerance(ts, now, g.tolerance) {
		return Result{}, fail("timestamp outside tolerance")
	}

	res := Result{}
	if g.deliveryIDHdr != "" {
		res.DeliveryID = cleanID(r.Header.Get(g.deliveryIDHdr), maxIDLen)
	}
	return res, nil
}
