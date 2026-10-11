// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

// Package api is generated from docs/openapi.json: a Go type for every schema,
// and a StrictServerInterface with one method per operation. Each method returns
// one of the response types its operation documents, so a status the document
// does not list is a compile error, not a surprise.
//
// Regenerate with `make generate` after editing the document. CI fails if the
// generated file is out of date. The generator is pinned here and is not a
// dependency of the module.
package api

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config oapi.yaml ../../docs/openapi.json
