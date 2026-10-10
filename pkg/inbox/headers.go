// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import (
	"net/http"
	"strings"
)

// AlwaysRedactedHeaders are request headers that are never persisted, no
// matter what the inbox's verifier is. A Store drops them in Append even if
// the caller forgot to. The inbox's own shared-token header is also redacted,
// but only the ingest handler knows its name, so the handler removes it with
// RedactHeaders before calling Append.
var AlwaysRedactedHeaders = []string{"Authorization", "Cookie", "Proxy-Authorization"}

// RedactHeaders returns a copy of h in canonical header form without the
// always-redacted headers and without any header named in extra. Names are
// matched case-insensitively. h is not modified.
func RedactHeaders(h http.Header, extra ...string) http.Header {
	drop := make(map[string]struct{}, len(AlwaysRedactedHeaders)+len(extra))
	for _, name := range AlwaysRedactedHeaders {
		drop[http.CanonicalHeaderKey(name)] = struct{}{}
	}
	for _, name := range extra {
		if name = strings.TrimSpace(name); name != "" {
			drop[http.CanonicalHeaderKey(name)] = struct{}{}
		}
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		ck := http.CanonicalHeaderKey(k)
		if _, ok := drop[ck]; ok {
			continue
		}
		out[ck] = append(out[ck], vs...)
	}
	return out
}
