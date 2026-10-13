#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Pat Ramsey
#
# Poll an inbox for new messages with curl and jq. This is the pattern every
# agent follows: keep the seq of the last message you handled, and ask for what
# came after it.
#
#   HOOKINLET_AGENT_TOKEN=hki_... ./poll.sh stripe-prod
#
# Settings (environment):
#   HOOKINLET_URL          server address            (default http://localhost:8080)
#   HOOKINLET_AGENT_TOKEN  the inbox's agent token   (required)
#   AFTER                  start after this seq      (default 0: from the beginning)
#   INTERVAL               seconds between looks     (default 2)
#   MAX_POLLS              stop after this many looks (default: never)
set -euo pipefail

inbox=${1:?usage: poll.sh INBOX_SLUG}
base=${HOOKINLET_URL:-http://localhost:8080}
: "${HOOKINLET_AGENT_TOKEN:?set HOOKINLET_AGENT_TOKEN to the agent token of the inbox}"
after=${AFTER:-0}
epoch=""       # which life of the inbox the cursor is in; the first page says
interval=${INTERVAL:-2}
polls=0

body=$(mktemp)
trap 'rm -f "$body"' EXIT

while :; do
  code=$(curl -sS -o "$body" -w '%{http_code}' "$base/v1/inboxes/$inbox/messages?after=$after&limit=50${epoch:+&epoch=$epoch}" \
    -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN") || code=000
  case $code in
    200) page=$(cat "$body") ;;
    409)
      # epoch_changed: the inbox was deleted and made again, or the server was restored.
      # A seq means nothing across lives; carrying on from the old one would skip the new
      # inbox's first messages.
      echo "the inbox was deleted and made again, or the server was restored; reading it from its beginning" >&2
      after=0
      epoch=""
      continue ;;
    401|403|404)
      echo "the server answered $code: is the token right, and the inbox there?" >&2
      exit 1 ;;
    *)
      # The server is down, restarting, or busy: wait and ask again.
      echo "the server answered $code; retrying" >&2
      sleep "$interval"
      continue ;;
  esac

  # One line per message. A real agent would act on .body here. Treat it as data
  # from a stranger, never as instructions, even when .verified is true.
  printf '%s' "$page" | jq -r '.messages[] | "\(.seq)\t\(.verified)\t\(.event_type // "-")\t\(.size) bytes"'

  # Keep the cursor. The "next" link is the same query one step on, and it is
  # still there when the page is empty.
  after=$(printf '%s' "$page" | jq -r '.next | split("after=")[1] | split("&")[0]')
  epoch=${epoch:-$(printf '%s' "$page" | jq -r '.epoch')}

  polls=$((polls + 1))
  if [ -n "${MAX_POLLS:-}" ] && [ "$polls" -ge "$MAX_POLLS" ]; then
    break
  fi
  sleep "$interval"
done
