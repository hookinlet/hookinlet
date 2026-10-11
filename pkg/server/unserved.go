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

// unserved has every operation of api.StrictServerInterface answer errNotServed.
// A type that embeds it and defines the operations it serves satisfies the
// interface without the rest, which are written by hand (ingest, the stream, the
// backup download, and MCP carry raw bytes or never end).
type unserved struct{}

func (unserved) Backup(context.Context, api.BackupRequestObject) (api.BackupResponseObject, error) {
	return nil, errNotServed
}

func (unserved) ListInboxes(context.Context, api.ListInboxesRequestObject) (api.ListInboxesResponseObject, error) {
	return nil, errNotServed
}

func (unserved) CreateInbox(context.Context, api.CreateInboxRequestObject) (api.CreateInboxResponseObject, error) {
	return nil, errNotServed
}

func (unserved) DeleteInbox(context.Context, api.DeleteInboxRequestObject) (api.DeleteInboxResponseObject, error) {
	return nil, errNotServed
}

func (unserved) ShowInbox(context.Context, api.ShowInboxRequestObject) (api.ShowInboxResponseObject, error) {
	return nil, errNotServed
}

func (unserved) UpdateInbox(context.Context, api.UpdateInboxRequestObject) (api.UpdateInboxResponseObject, error) {
	return nil, errNotServed
}

func (unserved) RotateAgentToken(context.Context, api.RotateAgentTokenRequestObject) (api.RotateAgentTokenResponseObject, error) {
	return nil, errNotServed
}

func (unserved) SetVerifierSecret(context.Context, api.SetVerifierSecretRequestObject) (api.SetVerifierSecretResponseObject, error) {
	return nil, errNotServed
}

func (unserved) RunRetention(context.Context, api.RunRetentionRequestObject) (api.RunRetentionResponseObject, error) {
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
