-- Move the message body to the end of the row.
--
-- SQLite keeps a row's columns in order in one record, and a record bigger than
-- a page spills onto overflow pages. With the body in the middle, reading any
-- column after it (size, verified, delivery_id, event_type, source_ip) meant
-- following the chain of overflow pages through the whole body, so a page of
-- summaries cost nearly as much as a page of bodies. With the body last, every
-- other column sits at the front of the record and a read that does not want the
-- body never touches an overflow page.
--
-- The table is rebuilt, so this takes time and disk space in proportion to the
-- size of the database (a copy of the database is kept before it runs).
CREATE TABLE messages_new (
  id           INTEGER PRIMARY KEY,
  inbox_id     INTEGER NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  seq          INTEGER NOT NULL,
  received_at  TEXT    NOT NULL,
  method       TEXT    NOT NULL,
  path_suffix  TEXT    NOT NULL DEFAULT '',
  query        TEXT    NOT NULL DEFAULT '',
  headers      TEXT    NOT NULL,
  content_type TEXT    NOT NULL DEFAULT '',
  size         INTEGER NOT NULL,
  verified     INTEGER NOT NULL,
  verify_error TEXT,
  delivery_id  TEXT,
  event_type   TEXT,
  source_ip    TEXT,
  dedupe_key   TEXT,
  body         BLOB    NOT NULL,                         -- last, so nothing sits behind it
  UNIQUE (inbox_id, seq)
);

INSERT INTO messages_new (id, inbox_id, seq, received_at, method, path_suffix, query, headers, content_type,
                          size, verified, verify_error, delivery_id, event_type, source_ip, dedupe_key, body)
  SELECT id, inbox_id, seq, received_at, method, path_suffix, query, headers, content_type,
         size, verified, verify_error, delivery_id, event_type, source_ip, dedupe_key, body
  FROM messages ORDER BY id;

DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;

CREATE INDEX messages_inbox_type ON messages(inbox_id, event_type, seq);
CREATE INDEX messages_inbox_received ON messages(inbox_id, received_at);
CREATE UNIQUE INDEX messages_dedupe ON messages(inbox_id, dedupe_key)
  WHERE dedupe_key IS NOT NULL AND verified = 1;
CREATE INDEX messages_inbox_delivery ON messages(inbox_id, delivery_id, seq)
  WHERE delivery_id IS NOT NULL;
