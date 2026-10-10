-- Look up messages by delivery id without scanning the inbox.
--
-- messages_dedupe covers only verified messages (it enforces deduplication), so
-- a query for "the message with delivery id X" that does not also say
-- "verified" cannot use it. At a million messages that query was a full scan
-- of about 800 ms; with this index it is a lookup.
--
-- seq is the last column so the index also yields rows in seq order. Without it
-- SQLite prefers walking the (inbox_id, seq) index to avoid a sort, and
-- filters every row on the way.
CREATE INDEX messages_inbox_delivery ON messages(inbox_id, delivery_id, seq)
  WHERE delivery_id IS NOT NULL;
