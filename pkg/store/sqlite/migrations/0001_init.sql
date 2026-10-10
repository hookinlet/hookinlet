-- Initial schema. Migrations are append-only: never edit a shipped migration,
-- add a new numbered file instead.

CREATE TABLE inboxes (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT, -- AUTOINCREMENT: ids are never reused
  slug                TEXT    NOT NULL UNIQUE,           -- [a-z0-9-]{3,64}; OSS ingest key
  name                TEXT    NOT NULL,
  version             INTEGER NOT NULL DEFAULT 1,        -- bumped by every successful update
  verifier            TEXT    NOT NULL,
  verifier_config     TEXT    NOT NULL DEFAULT '{}',     -- JSON object, non-secret params
  verifier_secret     BLOB,                              -- SecretBox ciphertext
  on_verify_fail      TEXT    NOT NULL DEFAULT 'reject', -- reject|store
  agent_token_hash    BLOB    NOT NULL UNIQUE,           -- hash of the agent token
  max_body_bytes      INTEGER NOT NULL DEFAULT 1048576,
  retention_days      INTEGER NOT NULL DEFAULT 0,        -- 0 = no age limit
  retention_max_count INTEGER NOT NULL DEFAULT 0,        -- 0 = no count limit
  event_type_pointer  TEXT    NOT NULL DEFAULT '',       -- JSON pointer, '' = none
  last_seq            INTEGER NOT NULL DEFAULT 0,
  disabled            INTEGER NOT NULL DEFAULT 0,
  created_at          TEXT    NOT NULL,                  -- fixed-width UTC, microseconds
  updated_at          TEXT    NOT NULL
);

CREATE TABLE messages (
  id           INTEGER PRIMARY KEY,                      -- internal only, never exposed
  inbox_id     INTEGER NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  seq          INTEGER NOT NULL,                         -- the cursor
  received_at  TEXT    NOT NULL,                         -- fixed-width UTC, microseconds
  method       TEXT    NOT NULL,
  path_suffix  TEXT    NOT NULL DEFAULT '',
  query        TEXT    NOT NULL DEFAULT '',
  headers      TEXT    NOT NULL,                         -- JSON, already redacted
  content_type TEXT    NOT NULL DEFAULT '',
  body         BLOB    NOT NULL,                         -- exact bytes received
  size         INTEGER NOT NULL,
  verified     INTEGER NOT NULL,
  verify_error TEXT,
  delivery_id  TEXT,
  event_type   TEXT,
  source_ip    TEXT,
  UNIQUE (inbox_id, seq)
);

CREATE INDEX messages_inbox_type ON messages(inbox_id, event_type, seq);
-- received_at never decreases with seq inside an inbox, so this serves both
-- time-range queries and age-based retention.
CREATE INDEX messages_inbox_received ON messages(inbox_id, received_at);
-- Only verified deliveries are deduplicated, so a forged delivery cannot claim
-- a real delivery id and suppress the genuine one.
CREATE UNIQUE INDEX messages_dedupe ON messages(inbox_id, delivery_id)
  WHERE delivery_id IS NOT NULL AND verified = 1;
