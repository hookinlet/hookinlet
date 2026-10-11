-- The key in an ingest URL (/in/{key}) is no longer the slug: it is its own
-- column, random for new inboxes, so a guessable slug is not a capability.
-- Existing inboxes keep working at the URL they already have.
ALTER TABLE inboxes ADD COLUMN ingest_key TEXT;
UPDATE inboxes SET ingest_key = slug;
CREATE UNIQUE INDEX inboxes_ingest_key ON inboxes(ingest_key);

-- Deduplication is by dedupe_key, which is the delivery id when the signature
-- covers it, and the delivery id together with a hash of the body when it does
-- not. A captured request therefore cannot claim ids of deliveries that have not
-- happened yet.
ALTER TABLE messages ADD COLUMN dedupe_key TEXT;
UPDATE messages SET dedupe_key = delivery_id WHERE delivery_id IS NOT NULL AND verified = 1;
DROP INDEX messages_dedupe;
CREATE UNIQUE INDEX messages_dedupe ON messages(inbox_id, dedupe_key)
  WHERE dedupe_key IS NOT NULL AND verified = 1;
