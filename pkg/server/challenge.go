// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"errors"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// defaultChallenge is the WWW-Authenticate value of a 401 when the Authenticator
// does not supply one (see inbox.Challenger).
const defaultChallenge = `Bearer realm="hookinlet"`

// maxChallengeBytes bounds a challenge an Authenticator supplies.
const maxChallengeBytes = 4096

// challengeFor is the WWW-Authenticate value for a 401 caused by err: the one the
// Authenticator's error supplies through inbox.Challenger if it is a valid
// challenge, and the server's own otherwise. A supplied value that is not valid is
// logged, once.
func (s *Server) challengeFor(err error) string {
	var c inbox.Challenger
	if !errors.As(err, &c) {
		return defaultChallenge
	}
	v := c.WWWAuthenticate()
	if validChallenge(v) {
		return v
	}
	s.badChallenge.Do(func() {
		s.log.Warn("ignoring the WWW-Authenticate value an Authenticator supplied: it must be one line of printable ASCII, at most 4096 bytes, starting with an authentication scheme such as Bearer; the default is sent instead")
	})
	return defaultChallenge
}

// validChallenge reports whether v can be sent as a WWW-Authenticate value: one
// line of printable ASCII (space allowed; no control character, so no CR, LF or
// tab), not empty, no longer than maxChallengeBytes, that starts with an
// authentication scheme (an RFC 9110 token) and then ends or goes on after a
// space. The parameters after the scheme are not parsed.
func validChallenge(v string) bool {
	if v == "" || len(v) > maxChallengeBytes {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < ' ' || v[i] > '~' {
			return false
		}
	}
	n := 0
	for n < len(v) && isTokenChar(v[n]) {
		n++
	}
	return n > 0 && (n == len(v) || v[n] == ' ')
}

// isTokenChar reports whether c is a tchar of RFC 9110.
func isTokenChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
