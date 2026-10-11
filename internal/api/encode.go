// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
)

// encodeJSON is how the generated code writes a JSON response body (see
// templates/strict-interface.tmpl). HTML characters are left alone and there is
// no trailing newline, so a sender's JSON body, carried as raw JSON, reaches the
// reader as the bytes it sent instead of an equivalent text with `<` rewritten
// as <.
func encodeJSON(buf *bytes.Buffer, v any) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(len(bytes.TrimRight(buf.Bytes(), "\n")))
	return nil
}

// setContentLength states the length of a body that is already encoded, so a
// large response is not sent chunked.
func setContentLength(w http.ResponseWriter, buf *bytes.Buffer) {
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
}

// EncodedLength is the length of v as the generated code would write it, for a
// HEAD response that has to say how long the GET's answer would have been.
func EncodedLength(v any) (int, error) {
	var buf bytes.Buffer
	if err := encodeJSON(&buf, v); err != nil {
		return 0, err
	}
	return buf.Len(), nil
}
