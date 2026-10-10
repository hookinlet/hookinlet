// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP returns the address a request came from, as text, or "" if it
// cannot be determined.
//
// X-Forwarded-For is believed only when the immediate peer is a trusted proxy.
// Even then the header is read from the right: each hop appended by a trusted
// proxy is skipped, and the first address that is not a trusted proxy is the
// client. Anything to the left of that was supplied by the client and could be
// anything, so it is never used. If the header is malformed, the peer address
// is returned.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, ok := peerAddr(r.RemoteAddr)
	if !ok {
		return ""
	}
	if len(trusted) == 0 || !containsAddr(trusted, peer) {
		return peer.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, p := range strings.Split(v, ",") {
			hops = append(hops, strings.TrimSpace(p))
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			return peer.String()
		}
		addr = addr.Unmap().WithZone("")
		if !containsAddr(trusted, addr) {
			return addr.String()
		}
	}
	return peer.String()
}

func peerAddr(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

func containsAddr(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
