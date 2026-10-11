#!/usr/bin/env bash
# End-to-end smoke test of a built image. It creates an inbox in a fresh data
# volume, starts the server, sends signed, tampered, and malformed requests,
# restarts the container, and checks that stored data and sequence numbers
# survive.
#
# Usage: scripts/smoke-docker.sh IMAGE
set -euo pipefail

image=${1:?usage: smoke-docker.sh IMAGE}
suffix=$$
vol=hookinlet-smoke-$suffix
name=hookinlet-smoke-$suffix
port=${SMOKE_PORT:-18090}
base=http://127.0.0.1:$port
key=$(openssl rand -base64 32)
secret='smoke-shared-secret'
tmp=$(mktemp)

stream_pids=()
cleanup() {
  for pid in "${stream_pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker volume rm "$vol" >/dev/null 2>&1 || true
  rm -f "$tmp" "${stream_files[@]}"
}
stream_files=()
trap cleanup EXIT

fail() {
  echo "SMOKE TEST FAILED: $*" >&2
  docker logs "$name" 2>&1 | tail -20 >&2 || true
  exit 1
}

expect() { # expect WHAT WANT GOT
  [ "$2" = "$3" ] || fail "$1: want '$2', got '$3'"
  echo "ok   $1"
}

# read PATH TOKEN  ->  prints "<status> <body>" for an authenticated GET
read_api() {
  local path=$1 tok=${2-}
  local args=(-sS -o "$tmp" -w '%{http_code}' "$base$path")
  [ -n "$tok" ] && args+=(-H "Authorization: Bearer $tok")
  local code
  code=$(curl "${args[@]}")
  echo "$code $(cat "$tmp")"
}

# send METHOD PATH BODY [SIGNATURE]  ->  prints "<status> <body>"
send() {
  local method=$1 path=$2 body=$3 sig=${4-}
  local args=(-sS -o "$tmp" -w '%{http_code}' -X "$method" "$base$path" --data-binary "$body")
  [ -n "$sig" ] && args+=(-H "X-Signature: sha256=$sig")
  local code
  code=$(curl "${args[@]}")
  echo "$code $(cat "$tmp")"
}

sign() { printf '%s' "$1" | openssl dgst -sha256 -hmac "$secret" -hex | sed 's/^.* //'; }

# wait_for FILE PATTERN [SECONDS]: wait until the file contains the pattern.
wait_for() {
  local file=$1 pattern=$2 limit=${3:-5}
  for _ in $(seq 1 $((limit * 10))); do
    if grep -q -- "$pattern" "$file" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  return 1
}

# follow PATH [LAST_EVENT_ID]: start streaming into a fresh file; sets $stream_file and $stream_pid.
follow() {
  local path=$1 last=${2-}
  stream_file=$(mktemp)
  stream_files+=("$stream_file")
  local args=(-sN -H "Authorization: Bearer $agent_token")
  [ -n "$last" ] && args+=(-H "Last-Event-ID: $last")
  curl "${args[@]}" "$base$path" -o "$stream_file" &
  stream_pid=$!
  stream_pids+=("$stream_pid")
  wait_for "$stream_file" "retry: 3000" 5 || fail "the stream never opened"
}

wait_ready() {
  for _ in $(seq 1 40); do
    if curl -fsS "$base/readyz" >/dev/null 2>&1; then return 0; fi
    sleep 0.25
  done
  fail "the server never became ready"
}

docker volume create "$vol" >/dev/null
run() { docker run --rm -v "$vol:/data" -e HOOKINLET_MASTER_KEY="$key" "$@"; }

run "$image" migrate >/dev/null
created=$(run -e HOOKINLET_VERIFIER_SECRET="$secret" "$image" inbox create --slug demo --verifier hmac \
  --verifier-config '{"header":"X-Signature","prefix":"sha256="}')
agent_token=$(printf '%s\n' "$created" | sed -n 's/^ *agent token: *//p')
[ -n "$agent_token" ] || fail "inbox create did not print an agent token"
other=$(run "$image" inbox create --slug other-inbox)
other_token=$(printf '%s\n' "$other" | sed -n 's/^ *agent token: *//p')
[ -n "$other_token" ] || fail "inbox create did not print a second agent token"

docker run -d --name "$name" -p "$port:8080" -v "$vol:/data" \
  -e HOOKINLET_ADMIN_TOKEN=smoke-admin-token-0123456789-0123456789 -e HOOKINLET_MASTER_KEY="$key" "$image" >/dev/null
wait_ready

body1='{"hello":"world"}'
body2='{"second":true}'
expect "a signed delivery is accepted"      '200 {"seq":1}' "$(send POST /in/demo "$body1" "$(sign "$body1")")"
expect "a second delivery gets the next seq" '200 {"seq":2}' "$(send PUT /in/demo/sub/path "$body2" "$(sign "$body2")")"
expect "a tampered body is rejected"        '401' "$(send POST /in/demo "$body2" "$(sign "$body1")" | cut -d' ' -f1)"
expect "a missing signature is rejected"    '401' "$(send POST /in/demo "$body1" | cut -d' ' -f1)"
expect "GET is not allowed"                 '405' "$(send GET /in/demo "" | cut -d' ' -f1)"
expect "an unknown inbox is 404"            '404' "$(send POST /in/nobody "$body1" "$(sign "$body1")" | cut -d' ' -f1)"
expect "liveness"                           'ok'  "$(curl -fsS "$base/healthz" | sed 's/.*"status":"\([a-z]*\)".*/\1/')"

# The read API, with the token that `inbox create` printed.
expect "no token is 401"                       '401' "$(read_api /v1/inboxes/demo | cut -d' ' -f1)"
expect "a wrong token is 401"                  '401' "$(read_api /v1/inboxes/demo hki_wrong | cut -d' ' -f1)"
expect "another inbox's token is 404"          '404' "$(read_api /v1/inboxes/demo "$other_token" | cut -d' ' -f1)"
expect "that is the same as an inbox that is not there" \
  "$(read_api /v1/inboxes/not-there "$other_token")" "$(read_api /v1/inboxes/demo "$other_token")"
expect "the agent token reads its inbox"       '200' "$(read_api /v1/inboxes/demo "$agent_token" | cut -d' ' -f1)"
info=$(read_api /v1/inboxes/demo "$agent_token")
case $info in *'"message_count":2'*'"latest_seq":2'*) echo "ok   the inbox reports two messages" ;; *) fail "unexpected inbox info: $info" ;; esac
page=$(read_api "/v1/inboxes/demo/messages?after=0" "$agent_token")
case $page in
  *'"seq":1'*'"seq":2'*'"next":"/v1/inboxes/demo/messages?after=2&limit=50"'*) echo "ok   the page lists both messages and a next link" ;;
  *) fail "unexpected page: $page" ;;
esac
case $page in *'"body":{"hello":"world"}'*) echo "ok   a JSON body is embedded as JSON" ;; *) fail "the JSON body was not embedded: $page" ;; esac
expect "the raw endpoint returns the exact bytes"  "200 $body1" "$(read_api /v1/inboxes/demo/messages/1/raw "$agent_token")"

# The MCP endpoint: the same reads as tools, with the same tokens.
# mcp TOKEN TOOL ARGS_JSON  ->  prints "<status> <body>" of one JSON-RPC tools/call
mcp() {
  local tok=$1 tool=$2 args=$3
  local curl_args=(-sS -o "$tmp" -w '%{http_code}' -X POST "$base/mcp"
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream'
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args}}")
  [ -n "$tok" ] && curl_args+=(-H "Authorization: Bearer $tok")
  local code
  code=$(curl "${curl_args[@]}")
  echo "$code $(cat "$tmp")"
}
expect "MCP without a token is 401"            '401' "$(mcp "" list_inboxes '{}' | cut -d' ' -f1)"
expect "MCP with a wrong token is 401"         '401' "$(mcp hki_wrong list_inboxes '{}' | cut -d' ' -f1)"
mcp_list=$(mcp "$agent_token" list_inboxes '{}')
case $mcp_list in *'"slug\":\"demo\"'*) echo "ok   MCP list_inboxes shows the token's inbox" ;; *) fail "unexpected list_inboxes: $mcp_list" ;; esac
case $mcp_list in *other-inbox*) fail "list_inboxes shows another inbox: $mcp_list" ;; *) echo "ok   and not anyone else's" ;; esac
mcp_query=$(mcp "$agent_token" query_messages '{"inbox":"demo","after":0}')
case $mcp_query in
  *'\"seq\":1'*'\"seq\":2'*'\"next_after\":2'*) echo "ok   MCP query_messages returns both messages and next_after" ;;
  *) fail "unexpected query_messages: $mcp_query" ;;
esac
case $(mcp "$agent_token" get_message '{"inbox":"demo","seq":1}') in *'\"hello\":\"world\"'*) echo "ok   MCP get_message returns the body" ;; *) fail "get_message did not return the body" ;; esac
foreign=$(mcp "$other_token" query_messages '{"inbox":"demo"}')
case $foreign in *'not found'*) echo "ok   MCP: another inbox's token is told not found" ;; *) fail "unexpected answer for a foreign inbox: $foreign" ;; esac
expect "MCP: that is the same as an inbox that is not there" \
  "$(mcp "$other_token" query_messages '{"inbox":"not-there"}')" "$foreign"
expect "MCP: batches are refused" '400' "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$base/mcp" \
  -H "Authorization: Bearer $agent_token" -H 'Content-Type: application/json' -d '[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]')"
case $(mcp "$agent_token" wait_for_messages '{"inbox":"demo","after":1,"timeout_seconds":5}') in
  *'\"seq\":2'*) echo "ok   MCP wait_for_messages returns at once when messages are waiting" ;; *) fail "wait_for_messages did not return message 2" ;; esac
case $(mcp "$agent_token" wait_for_messages '{"inbox":"demo","after":2,"timeout_seconds":1}') in
  *'\"messages\":[]'*'\"next_after\":2'*) echo "ok   and returns an empty list at its timeout" ;; *) fail "wait_for_messages did not time out cleanly" ;; esac

# The live stream: an event arrives as a delivery is stored.
follow "/v1/inboxes/demo/stream"
body3='{"live":true}'
expect "a delivery while streaming is accepted" '200 {"seq":3}' "$(send POST /in/demo "$body3" "$(sign "$body3")")"
wait_for "$stream_file" "id: 3" 5 || fail "the stream never delivered message 3: $(cat "$stream_file")"
grep -q "event: message" "$stream_file" || fail "the stream event has no event name"
grep -q '"body":{"live":true}' "$stream_file" || fail "the streamed event lacks the message: $(cat "$stream_file")"
echo "ok   the stream delivered the new message as an event"
case $(grep -c "^id: " "$stream_file") in 1) echo "ok   and only that one (no replay of older messages)" ;; *) fail "the stream replayed old messages: $(cat "$stream_file")" ;; esac

# Stopping the container ends open streams promptly (SIGTERM, then a clean exit).
begin=$(date +%s)
docker stop -t 15 "$name" >/dev/null
took=$(( $(date +%s) - begin ))
[ "$took" -le 5 ] || fail "stopping the container took ${took}s with a stream open"
wait "$stream_pid" 2>/dev/null || true
if kill -0 "$stream_pid" 2>/dev/null; then fail "the stream client is still connected after the container stopped"; fi
echo "ok   stopping the container ended the open stream (${took}s)"
docker start "$name" >/dev/null

# The data and the sequence counter survive a restart.
wait_ready
expect "the sequence continues after a restart" '200 {"seq":4}' "$(send POST /in/demo "$body1" "$(sign "$body1")")"
follow "/v1/inboxes/demo/stream" 3
wait_for "$stream_file" "id: 4" 5 || fail "reconnecting with Last-Event-ID 3 did not resume at 4: $(cat "$stream_file")"
case $(grep -c "^id: " "$stream_file") in 1) echo "ok   Last-Event-ID resumed exactly after message 3" ;; *) fail "unexpected replay: $(cat "$stream_file")" ;; esac
expect "messages are still there after a restart" "200" "$(read_api /v1/inboxes/demo/messages/2 "$agent_token" | cut -d' ' -f1)"
expect "and so is the raw body" "200 $body2" "$(read_api /v1/inboxes/demo/messages/2/raw "$agent_token")"

echo "smoke test passed"
