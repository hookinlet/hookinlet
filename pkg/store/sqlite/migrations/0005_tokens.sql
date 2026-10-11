-- An inbox no longer has exactly one agent token. Tokens are their own rows: any
-- number per inbox, each with a name, and each scoped to some inboxes or to all of
-- them, so that a token can be replaced by creating its successor before revoking
-- it, and one agent can read several inboxes with one credential.
CREATE TABLE tokens (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,  -- AUTOINCREMENT: ids are never reused
  name        TEXT    NOT NULL,
  hash        BLOB    NOT NULL UNIQUE,            -- hash of the token; the token is shown once
  all_inboxes INTEGER NOT NULL DEFAULT 0,         -- 1: reads every inbox, now and later
  created_at  TEXT    NOT NULL                    -- fixed-width UTC, microseconds
);

CREATE TABLE token_inboxes (
  token_id INTEGER NOT NULL REFERENCES tokens(id)  ON DELETE CASCADE,
  inbox_id INTEGER NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  PRIMARY KEY (token_id, inbox_id)
);
CREATE INDEX token_inboxes_inbox ON token_inboxes(inbox_id);

-- Each inbox's token becomes a token named "default" that reads that inbox.
INSERT INTO tokens (name, hash, all_inboxes, created_at)
  SELECT 'default', agent_token_hash, 0, created_at FROM inboxes ORDER BY id;
INSERT INTO token_inboxes (token_id, inbox_id)
  SELECT t.id, i.id FROM tokens t JOIN inboxes i ON i.agent_token_hash = t.hash;

-- inboxes.agent_token_hash is not used any more. SQLite cannot drop a column that
-- is UNIQUE, so it stays, filled with random bytes when an inbox is created.
