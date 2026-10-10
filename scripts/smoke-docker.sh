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

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker volume rm "$vol" >/dev/null 2>&1 || true
  rm -f "$tmp"
}
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

# The data and the sequence counter survive a restart.
docker restart "$name" >/dev/null
wait_ready
expect "the sequence continues after a restart" '200 {"seq":3}' "$(send POST /in/demo "$body1" "$(sign "$body1")")"
expect "messages are still there after a restart" "200" "$(read_api /v1/inboxes/demo/messages/2 "$agent_token" | cut -d' ' -f1)"
expect "and so is the raw body" "200 $body2" "$(read_api /v1/inboxes/demo/messages/2/raw "$agent_token")"

echo "smoke test passed"
