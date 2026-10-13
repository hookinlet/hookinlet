#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Pat Ramsey
#
# Runs the examples in the documentation, so that they cannot rot.
#
# A fenced ```sh block that is preceded by the line
#
#     <!-- exercise -->
#
# (or <!-- exercise-server -->, which says the server must be running from this
# block on) is run, in order with the other marked blocks of the same file, in one shell
# with `set -e`: a command that fails fails the test. Each file gets a fresh
# server on a free port (the documents say localhost:8080; it is rewritten), a
# fresh data directory, and a `hookinlet` built from this checkout on the PATH.
# A document that starts the server itself (a block with `hookinlet serve`) is
# left to; for every other file the server is started before the first block,
# or before the first <!-- exercise-server --> block if there is one. Docker
# commands are covered by scripts/smoke-docker.sh, not here.
#
# Usage: scripts/test-docs.sh [FILE.md ...]    (default: every document that has marked blocks)
#        HOOKINLET_BIN=/path/to/hookinlet scripts/test-docs.sh   (skip the build)
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
files=("$@")
[ ${#files[@]} -gt 0 ] || files=(README.md examples/README.md docs/verifiers.md docs/mcp.md docs/admin.md)

# The examples write a few files into the directory they are run from, which is the
# checkout (they run `go run ./examples/...`). Refuse to start if any of them is
# already there, so that nothing of yours can be overwritten, and remove only
# what this script made.
scratch_files=(created.json relay.state received.log hookinlet-backup.db relay inboxes.yaml tokens.json restore-test.db restored)
for f in "${scratch_files[@]}"; do
  if [ -e "$root/$f" ]; then
    echo "refusing to run: $root/$f already exists and the examples would overwrite it. Move it away first." >&2
    exit 2
  fi
done

work=$(mktemp -d)
cleanup() {
  rm -rf "$work"
  for f in "${scratch_files[@]}"; do rm -rf "${root:?}/$f"; done   # none of these existed before the run (checked above)
}
trap cleanup EXIT

# A document that hangs must fail, not wait for ever.
limit=${DOCS_TEST_TIMEOUT:-240}

if [ -n "${HOOKINLET_BIN:-}" ]; then
  mkdir -p "$work/bin" && ln -s "$HOOKINLET_BIN" "$work/bin/hookinlet"
else
  CGO_ENABLED=0 go build -o "$work/bin/hookinlet" ./cmd/hookinlet
fi

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
PY
}

# extract FILE -> prints the marked blocks, one after another.
extract() {
  awk '
    /^<!-- exercise(-server)? -->[[:space:]]*$/ { armed = 1; if ($0 ~ /server/) print "#SERVER"; next }
    armed && /^```(sh|bash)[[:space:]]*$/ { inblock = 1; armed = 0; next }
    inblock && /^```[[:space:]]*$/ { inblock = 0; print ""; next }
    inblock { print; next }
    { armed = 0 }
  ' "$1"
}

status=0
for file in "${files[@]}"; do
  blocks=$(extract "$file")
  if [ -z "$blocks" ]; then
    echo "skip  $file (no marked blocks)"
    continue
  fi
  port=$(free_port)
  script="$work/$(echo "$file" | tr '/.' '__').sh"
  {
    echo 'set -euo pipefail'
    echo 'trap '"'"'kill $(jobs -p) 2>/dev/null || true'"'"' EXIT TERM INT'
    echo 'export HOOKINLET_ADMIN_TOKEN=$(openssl rand -base64 32)'
    start_server() {
      echo 'export HOOKINLET_MASTER_KEY=${HOOKINLET_MASTER_KEY:-$(openssl rand -base64 32)}'
      echo 'hookinlet serve &'
      echo 'until curl -sf localhost:8080/readyz >/dev/null; do sleep 0.2; done'
      case "$file" in
      docs/mcp.md) # the document talks about an inbox called demo and its agent token
        echo 'export HOOKINLET_AGENT_TOKEN=$(curl -s -X POST localhost:8080/v1/admin/inboxes -H "Authorization: Bearer $HOOKINLET_ADMIN_TOKEN" -H "Content-Type: application/json" -d "{\"slug\": \"demo\", \"ingest_key\": \"demo\", \"verifier\": \"none\"}" | sed -n '"'"'s/.*"agent_token":"\([^"]*\)".*/\1/p'"'"')'
        echo 'curl -s -X POST localhost:8080/in/demo -d "{\"n\":1}" >/dev/null'
        ;;
      esac
    }
    if printf '%s' "$blocks" | grep -q 'hookinlet serve'; then
      :                       # the document starts the server itself
    elif printf '%s' "$blocks" | grep -q '^#SERVER$'; then
      started=
      while IFS= read -r line; do
        if [ "$line" = '#SERVER' ]; then
          [ -n "$started" ] || { start_server; started=1; }
        else
          printf '%s\n' "$line"
        fi
      done <<< "$blocks"
      blocks=
    else
      start_server
    fi
    printf '%s\n' "$blocks"
  } | sed -e "s#localhost:8080#localhost:$port#g" -e "s#127.0.0.1:8080#127.0.0.1:$port#g" -e "s#https://hooks.example.com#http://localhost:$port#g" > "$script"

  # What each document must visibly have done, beyond exiting successfully. These
  # are the claims the document makes about its own commands, checked in the
  # output, and (for the README) in the state the server is left in.
  needles=()
  checks=
  case "$file" in
  README.md)
    needles=('{"seq":1}' 'verification_failed' '"verified":true' '"event_type":"pull_request"' 'structuredContent')
    checks='
n=$(curl -s localhost:8080/v1/inboxes/github-demo -H "Authorization: Bearer $HOOKINLET_AGENT_TOKEN" | sed -n '"'"'s/.*"message_count":\([0-9]*\).*/\1/p'"'"')
[ "$n" = 1 ] || { echo "CHECK: the walk should leave exactly one message (the tampered request must be refused), found: $n" >&2; exit 1; }
[ "$(curl -s -o /dev/null -w "%{http_code}" -X POST localhost:8080$INGEST -H "X-Hub-Signature-256: sha256=00" -d x)" = 401 ] || { echo "CHECK: a wrong signature was not refused" >&2; exit 1; }'
    ;;
  examples/README.md)
    needles=('seq=1 {"hello": "examples"}')
    checks='
grep -q '"'"'seq=1 {"hello": "examples"}'"'"' received.log || { echo "CHECK: the relay did not deliver the message" >&2; exit 1; }'
    ;;
  docs/verifiers.md) needles=('{"seq":1}') ;;
  docs/mcp.md) needles=('"structuredContent"') ;;
  docs/admin.md)
    needles=('"agent_token"' 'created token' 'the database is sound' 'would create' '2 created, 0 updated, 0 unchanged, 2 secrets set' '0 created, 0 updated, 2 unchanged, 2 secrets set')
    checks='
# apply must have written the new tokens, privately.
[ "$(find tokens.json -perm 600 | wc -l)" -eq 1 ] || { echo "CHECK: tokens.json is missing or is not mode 0600" >&2; exit 1; }
grep -q "hki_" tokens.json || { echo "CHECK: tokens.json holds no token" >&2; exit 1; }
curl -s localhost:8080/v1/admin/inboxes/stripe-live -H "Authorization: Bearer $HOOKINLET_ADMIN_TOKEN" | grep -q "\"has_verifier_secret\":true" || { echo "CHECK: apply did not set the secret" >&2; exit 1; }'
    ;;
  esac
  if [ -n "$checks" ]; then
    port2=$(free_port)   # keep the checks' port rewrite in step with the blocks'
    printf '%s\n' "$checks" | sed -e "s#localhost:8080#localhost:$port#g" >> "$script"
  fi
  # A receiver for the relay example needs a port of its own.
  rport=$(free_port)
  sed -i.bak -e "s#9099#$rport#g" "$script" && rm -f "$script.bak"

  log="$work/$(basename "$script").log"
  echo "run   $file ($(grep -c '^<!-- exercise' "$file") blocks, port $port)"
  run_script() {
    # The caller's own settings must not leak in: a HOOKINLET_URL exported for a
    # real server would send these commands there.
    for v in $(env | sed -n 's/^\(HOOKINLET_[A-Z_]*\)=.*/\1/p'); do unset "$v"; done
    export PATH="$work/bin:$PATH"
    export HOOKINLET_DATA_DIR="$work/data-$port" HOOKINLET_LISTEN_ADDR="127.0.0.1:$port"
    # The example clients default to localhost:8080; the commands in other files
    # (the inbox command, direct or remote) are chosen by the document itself.
    case "$file" in README.md|examples/*) export HOOKINLET_URL="http://localhost:$port" ;; esac
    export HOOKINLET_LOG_LEVEL=warn MAX_POLLS=1 INTERVAL=1 BASE="http://localhost:$port"
    bash "$script"
  }
  ( run_script ) > "$log" 2>&1 &
  runner=$!
  waited=0
  while kill -0 "$runner" 2>/dev/null; do
    if [ "$waited" -ge $((limit * 5)) ]; then
      echo "TIMEOUT after ${limit}s: $file" >&2
      pkill -TERM -P "$runner" 2>/dev/null || true
      kill -TERM "$runner" 2>/dev/null || true
      break
    fi
    sleep 0.2
    waited=$((waited + 1))
  done
  ok=0
  wait "$runner" || ok=1
  for needle in "${needles[@]}"; do
    grep -qF -- "$needle" "$log" || { echo "MISSING from the output of $file: $needle" >&2; ok=1; }
  done
  if [ "$ok" -eq 0 ]; then
    echo "ok    $file"
  else
    echo "FAIL  $file; the script and its output:" >&2
    sed 's/^/    | /' "$script" >&2
    echo "  --- output ---" >&2
    sed 's/^/    | /' "$log" >&2
    status=1
  fi
done
exit $status
