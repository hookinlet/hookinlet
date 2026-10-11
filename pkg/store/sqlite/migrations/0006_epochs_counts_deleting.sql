-- Three things an inbox needs to keep its cursors honest.
--
-- epoch: a random value that identifies one lifetime of an inbox's sequence
-- numbers. A new inbox gets its own, and a snapshot gets fresh ones (see
-- Backup), so a cursor from before a restore is told apart from one after it.
ALTER TABLE inboxes ADD COLUMN epoch TEXT NOT NULL DEFAULT '';
UPDATE inboxes SET epoch = lower(hex(randomblob(16)));

-- message_count: kept as messages come and go, so that it does not depend on the
-- stored sequence numbers being one contiguous range.
ALTER TABLE inboxes ADD COLUMN message_count INTEGER NOT NULL DEFAULT 0;
UPDATE inboxes SET message_count = (SELECT COUNT(*) FROM messages WHERE messages.inbox_id = inboxes.id);

-- deleting: set when a delete begins. Such an inbox is gone as far as every
-- reader is concerned, and whoever finds it (the retention sweep, or a second
-- DeleteInbox) finishes the job.
ALTER TABLE inboxes ADD COLUMN deleting INTEGER NOT NULL DEFAULT 0;
