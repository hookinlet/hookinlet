#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Pat Ramsey
#
# Follow an inbox live over Server-Sent Events. Each message arrives as it is
# stored; curl prints the raw event stream. A client that reconnects with the
# Last-Event-ID of the last event it saw receives every later message exactly
# once, so pass LAST_EVENT_ID to resume.
#
#   HOOKINLET_AGENT_TOKEN=hki_... ./stream.sh stripe-prod
#
# Settings: HOOKINLET_URL (default http://localhost:8080), HOOKINLET_AGENT_TOKEN
# (required), LAST_EVENT_ID (optional).
set -euo pipefail

inbox=${1:?usage: stream.sh INBOX_SLUG}
base=${HOOKINLET_URL:-http://localhost:8080}
: "${HOOKINLET_AGENT_TOKEN:?set HOOKINLET_AGENT_TOKEN to the agent token of the inbox}"

args=(-fsSN -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN")
if [ -n "${LAST_EVENT_ID:-}" ]; then
  args+=(-H "Last-Event-ID: $LAST_EVENT_ID")
fi
exec curl "${args[@]}" "$base/v1/inboxes/$inbox/stream"
