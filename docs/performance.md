# Performance

The read API's budget is a **p95 under 20 ms per request at one million stored
messages**. This page records what was measured, on what, and how to repeat it.

## Results

Measured on an Apple M1 Max (10 cores, 32 GB) with Go 1.27.2, against one inbox
holding 1,000,000 messages of about 300 bytes each, through the full HTTP
handler: authentication, inbox lookup, query, and JSON encoding. Each shape ran
400 times from random positions in the inbox.

| Request | p50 | p95 | max |
|---|---:|---:|---:|
| time window (`since` + `until`, one hour) | 10.8 ms | 11.4 ms | 28.7 ms |
| `since` a recent time | 10.0 ms | 10.5 ms | 20.1 ms |
| type + window + `verified=true` | 4.4 ms | 5.0 ms | 13.4 ms |
| `verified=false` (about 1% of messages) | 3.7 ms | 4.3 ms | 23.2 ms |
| `limit=500` | 2.9 ms | 3.3 ms | 14.8 ms |
| one event type | 0.46 ms | 0.74 ms | 1.0 ms |
| poll `after` a random cursor | 0.36 ms | 0.66 ms | 1.2 ms |
| page backward from a random cursor | 0.37 ms | 0.65 ms | 0.8 ms |
| `fields=summary` | 0.31 ms | 0.60 ms | 0.7 ms |
| poll `after` near the end | 0.33 ms | 0.60 ms | 0.7 ms |
| latest 50 | 0.34 ms | 0.59 ms | 0.8 ms |
| by `delivery_id` | 0.11 ms | 0.16 ms | 0.3 ms |
| one message, raw | 0.06 ms | 0.11 ms | 0.4 ms |
| one message | 0.06 ms | 0.10 ms | 0.2 ms |
| inbox info (includes the message count) | 0.05 ms | 0.07 ms | 0.2 ms |

The slowest shapes are the time-range filters. SQLite finds the range through the
`(inbox_id, received_at)` index and sorts it by `seq`; an hour is 3,600
messages here. The cost grows with how many messages fall inside the range, not
with the size of the inbox.

## What this does not measure

- **Writes.** Ingest throughput is separate. Each delivery is one durable
  transaction.
- **Many readers at once.** One request at a time was measured. Reads use their
  own pool of connections and do not wait for the writer.
- **A cold cache.** The first queries after a restart read from disk. The
  working set for the common shapes is the newest few pages.
- **Slower disks and smaller machines.** Expect the same shape, scaled.

## What was fixed to get here

The first run at a million messages found two reads far over budget:

| Read | Before | After | Cause and fix |
|---|---:|---:|---|
| lookup by `delivery_id` | 804 ms | 0.16 ms | A full scan: the only delivery-id index covers verified messages. Migration 0002 adds an index on `(inbox_id, delivery_id, seq)`, and the query says `delivery_id IS NOT NULL` so SQLite may use it. A test checks the query plan. |
| message count (`message_count`) | 140 ms | 0.07 ms | `COUNT(*)` over every row. Retention only ever removes a prefix, so the stored seqs are contiguous and the count is `latest - oldest + 1`. The conformance suite checks this against a real walk of the inbox. |

## Repeating the measurement

Seeding a million rows takes about 15 seconds. The measurements run only when
asked for:

```sh
# The full request path (what an agent experiences)
HOOKINLET_BENCH=1 go test -run TestAPILatencyAtScale -v -timeout 20m ./pkg/server/

# The store alone, every query shape
HOOKINLET_BENCH=1 go test -run TestQueryLatencyAtScale -v -timeout 20m ./pkg/store/sqlite/

# Go's usual ns/op for the common shapes
go test -run '^$' -bench BenchmarkQuery ./pkg/store/sqlite/
```

Both tests fail if any shape's p95 reaches 20 ms. `HOOKINLET_BENCH_ROWS`
changes the size of the inbox.
