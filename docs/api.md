# HTTP API

This document describes hookinlet's HTTP surface. Phase 2 provides ingest and
the operational endpoints; the agent query API, SSE, MCP, and the admin API are
added in later phases.

All error responses are JSON:

```json
{"error": {"code": "not_found", "message": "not found"}}
```

Every response carries an `X-Request-Id` header. If the request sent a
well-formed `X-Request-Id` (up to 64 characters from `A-Z a-z 0-9 - _ .`), it is
echoed; otherwise one is generated.

## Ingest

```
POST /in/{key}
PUT  /in/{key}
POST /in/{key}/{path...}
PUT  /in/{key}/{path...}
```

`{key}` is the inbox's slug. Anything after it is stored with the message as
`path`; the query string is stored as `query`. No authentication is required
beyond the inbox's [verifier](verifiers.md).

### Responses

| Status | Code | When |
|---|---|---|
| `200` | | The delivery was stored (or was a duplicate of one already stored). Body: `{"seq": 42}`. A Slack `url_verification` handshake also gets `200`, with the challenge as plain text. |
| `400` | `bad_request` | The request body could not be read. |
| `401` | `verification_failed` | The signature or token did not verify and the inbox's `on_verify_fail` is `reject`. |
| `402` | `quota_exceeded` | Refused by an embedder's hook (the open-source build never returns this). |
| `403` | `forbidden` | Refused by an embedder's hook. |
| `404` | `not_found` | No such inbox, or the inbox is disabled. The two are indistinguishable. |
| `405` | `method_not_allowed` | Any method other than POST and PUT. The `Allow` header is `POST, PUT`. Checked before the inbox is looked up. |
| `413` | `payload_too_large` | The body exceeds the inbox's `max_body_bytes` (default 1 MiB, at most 10 MiB). |
| `429` | `rate_limited` | Refused by an embedder's hook. |
| `500` | `internal` | A server-side problem, including a misconfigured verifier. Details are in the server log, never in the response. |

`seq` is the message's per-inbox sequence number, starting at 1. A duplicate
delivery returns the `seq` of the message that is already stored.

### Guarantees

- A `200` is sent **only after the message is committed** to the store.
- The body is stored byte for byte. Signatures are verified over those exact
  bytes before anything is parsed.
- `Authorization`, `Cookie`, `Proxy-Authorization`, and the inbox's shared-token
  header are never stored.
- The source IP is the connection's peer address. `X-Forwarded-For` is believed
  only when the peer is in `TRUSTED_PROXIES`, and then only the first address
  from the right that is not itself a trusted proxy.
- Retried deliveries are deduplicated when the verifier supplies a delivery id
  and the delivery verified; see [verifying deliveries](verifiers.md).

## Operations

```
GET /healthz   200 {"status": "ok"}     the process is up
GET /readyz    200 {"status": "ready"}  the store is reachable
               503 {"error": {"code": "not_ready", ...}} it is not
```

Neither requires authentication. Use `/healthz` for liveness checks and
`/readyz` for readiness.
