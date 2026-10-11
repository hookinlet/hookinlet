// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package server

import (
	"context"
	"errors"

	"github.com/hookinlet/hookinlet/internal/api"
)

// errNotServed is what an operation answers when it is reached through the
// generated interface although its handler is still written by hand. It cannot
// happen: only the operations a type implements are mounted.
var errNotServed = errors.New("server: this operation is not served through the generated interface")

// unserved fills in the operations that are not yet served through the generated
// interface, so that a type embedding it satisfies api.StrictServerInterface
// with only the operations it does serve.
type unserved struct{}

func (unserved) Backup(context.Context, api.BackupRequestObject) (api.BackupResponseObject, error) {
	return nil, errNotServed
}

func (unserved) Healthz(context.Context, api.HealthzRequestObject) (api.HealthzResponseObject, error) {
	return nil, errNotServed
}

func (unserved) Ingest(context.Context, api.IngestRequestObject) (api.IngestResponseObject, error) {
	return nil, errNotServed
}

func (unserved) IngestWithPath(context.Context, api.IngestWithPathRequestObject) (api.IngestWithPathResponseObject, error) {
	return nil, errNotServed
}

func (unserved) Mcp(context.Context, api.McpRequestObject) (api.McpResponseObject, error) {
	return nil, errNotServed
}

func (unserved) Readyz(context.Context, api.ReadyzRequestObject) (api.ReadyzResponseObject, error) {
	return nil, errNotServed
}

func (unserved) GetInbox(context.Context, api.GetInboxRequestObject) (api.GetInboxResponseObject, error) {
	return nil, errNotServed
}

func (unserved) HeadGetInbox(context.Context, api.HeadGetInboxRequestObject) (api.HeadGetInboxResponseObject, error) {
	return nil, errNotServed
}

func (unserved) ListMessages(context.Context, api.ListMessagesRequestObject) (api.ListMessagesResponseObject, error) {
	return nil, errNotServed
}

func (unserved) HeadListMessages(context.Context, api.HeadListMessagesRequestObject) (api.HeadListMessagesResponseObject, error) {
	return nil, errNotServed
}

func (unserved) GetMessage(context.Context, api.GetMessageRequestObject) (api.GetMessageResponseObject, error) {
	return nil, errNotServed
}

func (unserved) HeadGetMessage(context.Context, api.HeadGetMessageRequestObject) (api.HeadGetMessageResponseObject, error) {
	return nil, errNotServed
}

func (unserved) GetMessageRaw(context.Context, api.GetMessageRawRequestObject) (api.GetMessageRawResponseObject, error) {
	return nil, errNotServed
}

func (unserved) HeadGetMessageRaw(context.Context, api.HeadGetMessageRawRequestObject) (api.HeadGetMessageRawResponseObject, error) {
	return nil, errNotServed
}

func (unserved) StreamMessages(context.Context, api.StreamMessagesRequestObject) (api.StreamMessagesResponseObject, error) {
	return nil, errNotServed
}
