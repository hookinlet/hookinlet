// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package docs carries the OpenAPI document, so the server can hold itself to it.
package docs

import _ "embed"

// OpenAPI is docs/openapi.json, the contract of the HTTP API.
//
//go:embed openapi.json
var OpenAPI []byte
