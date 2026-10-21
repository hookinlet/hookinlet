# Performance

The read API has a budget: a **p95 under 20 ms per request at one million stored
messages**. (p95 means 95 out of 100 requests were at least this fast.) This page
shows what was measured, on what machine, and how to repeat it.

## Results

These runs used an Apple M1 Max (10 cores, 32 GB) and Go 1.27.2. They hit one
inbox that held 1,000,000 messages of about 300 bytes each. Each request went
through the full HTTP handler: authentication, inbox lookup, query, and JSON
encoding. Each request shape ran 400 times from random positions in the inbox.

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

We measured again after adding the page-size bounds below. The server now works
out each message's encoded size to enforce them. Nothing changed beyond noise,
and the worst p95 stayed at 11.4 ms.

The slowest requests are the time-range filters. SQLite finds the range through
the `(inbox_id, received_at)` index and sorts it by `seq`. An hour is 3,600
messages here. The cost grows with the number of messages in the range, not with
the size of the inbox.

## What this does not measure

- **Concurrent writes.** We did not measure ingest speed with many senders. Each
  delivery is one durable transaction. See "Cost of durable writes" below for
  one sender.
- **Many readers at once.** We sent one request at a time. Reads use their own
  pool of connections and do not wait for the writer.
- **A cold cache.** The first queries after a restart read from disk. The common
  requests touch only the newest few pages.
- **Slower disks and smaller machines.** Expect the same pattern, scaled.

## Cost of durable writes

Each commit is synced to disk (`synchronous=FULL`). A `2xx` from ingest means
the message survives power loss, not only a crash of the process. Earlier
versions used `synchronous=NORMAL`. That setting syncs the WAL only at
checkpoints, so power loss can lose the most recent commits.

`BenchmarkAppendSequential` is one sender that appends 1 KiB messages one after
another through `Store.Append`. It ran on the same machine as above (Go 1.27.2,
macOS, internal SSD), five runs of 3,000 appends each:

| `synchronous` | Per append (median of five runs) | Range | Appends per second |
|---|---:|---:|---:|
| NORMAL (before) | 98 us | 95 to 115 us | about 10,200 |
| FULL (now) | 134 us | 126 to 154 us | about 7,500 |

A sequential writer pays about 36 us more per message, roughly 1.4 times as
long. This is the figure for one machine and one disk. On a disk with slower
flushes, the difference is larger. On macOS, SQLite's default is `fsync`, which
does not force the drive's own cache to empty. Linux `fdatasync`/`fsync` does ask
the device to flush. The numbers say nothing about a host with a network volume.

We ran the append-latency gates for large deletes (below) again with FULL. They
still pass. Append p99 was 24 ms while one million rows are pruned (in another
inbox or the same one), 20 ms while a one-million-message inbox is deleted, and
17 ms while large messages are pruned. The gate is 50 ms. These p99 values are
higher than in the tables below (16 to 18 ms, and 9 ms for large messages). We
measured those tables with NORMAL and have not redone them.

```sh
go test -run '^$' -bench BenchmarkAppendSequential -benchtime 3000x ./pkg/store/sqlite/
```

## What was fixed to get here

The first run at a million messages found two reads far over budget.

| Read | Before | After | Cause and fix |
|---|---:|---:|---|
| lookup by `delivery_id` | 804 ms | 0.16 ms | A full scan: the only delivery-id index covers verified messages. Migration 0002 adds an index on `(inbox_id, delivery_id, seq)`, and the query says `delivery_id IS NOT NULL` so SQLite may use it. A test checks the query plan. |
| message count (`message_count`) | 140 ms | 0.07 ms | `COUNT(*)` over every row. Retention only ever removes a prefix, so the stored seqs are contiguous and the count is `latest - oldest + 1`. The conformance suite checks this against a real walk of the inbox. |

## Deliveries while a large prune runs

Retention (and deleting an inbox) removes messages in batches. All writes share
one connection, so a delivery that arrives meanwhile waits for the batch in
progress. A batch is the longest a delivery can wait. A batch holds at most 250
rows and at most 2 MiB of message bodies, whichever limit comes first, because
large messages make a batch slower. This test pruned one million rows (everything
but one message) while four senders appended every couple of milliseconds. It ran
on the same machine as the figures above:

| Rows per batch | Append p50 | Append p99 | Longest wait | The whole prune |
|---|---|---|---|---|
| 5000 | 35 ms | 279 ms | 592 ms | 9 s |
| 1000 | 6 ms | 48 ms | 222 ms | 9 s |
| 500 | 2 ms | 32 ms | 130 ms | 10 s |
| **250 (used)** | **2 ms** | **16 ms** | **66 ms** | **11 s** |
| 100 | 1 ms | 10 ms | 44 ms | 14 s |

The gate is a p99 under 50 ms. A batch of 250 rows leaves a wide margin. It adds
two seconds to a prune that nobody is waiting for. The next table counts only
appends made while the operation runs. We measured both tables with the earlier
`synchronous=NORMAL` setting. With the shipped FULL setting, the same operations
gave append p99 values of 24, 20 and 17 ms (see above). All are inside the 50 ms
gate:

| Operation | Append p99 | Longest wait |
|---|---|---|
| Prune 1,000,000 rows; appends to another inbox | 16 ms | 69 ms |
| The same; appends to the inbox being pruned | 18 ms | 74 ms |
| Delete a 1,000,000-message inbox (12 s) | 17 ms | 102 ms |
| Prune 1,500 messages of 512 KiB (0.6 s) | 9 ms | 15 ms |

We measured `Store.Append`, not the HTTP request around it. The request adds
verification and encoding, but nothing that waits for the writer. Each sender
sends one request, waits for the answer, pauses 2 ms, and repeats. A sender stuck
behind a batch sends nothing while it waits. With more senders, the queue behind
each batch is longer. The figures are for four senders.

## Repeating the measurement

Seeding a million rows takes about 15 seconds. The measurements run only when
you ask for them:

```sh
# The full request path (what an agent experiences)
HOOKINLET_BENCH=1 go test -run TestAPILatencyAtScale -v -timeout 20m ./pkg/server/

# The store alone, every query shape
HOOKINLET_BENCH=1 go test -run TestQueryLatencyAtScale -v -timeout 20m ./pkg/store/sqlite/

# Deliveries while a million rows are pruned or an inbox that size is deleted
HOOKINLET_BENCH=1 go test -run 'WhileA|WhileLarge' -v -timeout 20m ./pkg/store/sqlite/

# Go's usual ns/op for the common shapes
go test -run '^$' -bench BenchmarkQuery ./pkg/store/sqlite/
```

Both tests fail if any shape's p95 reaches 20 ms. `HOOKINLET_BENCH_ROWS`
changes the size of the inbox.
