-- message_modified: the messages whose stored body is not what the sender sent,
-- because the server's body screener (inbox.BodyScreener) replaced it. A signature
-- covered the original bytes, so a reader needs to know it cannot be checked against
-- this body.
--
-- It is a table of its own, holding a row only for a modified message, and not a
-- column of messages: a column added to messages sits behind the body, and reading
-- one after a large body means following its chain of overflow pages (see
-- 0007_body_last.sql), which every read of a page of messages would pay for. A
-- message with no row here, which is every message stored before this, is as the
-- sender sent it.
CREATE TABLE message_modified (
  message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE
) WITHOUT ROWID;
