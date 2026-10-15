-- Count a message's unverified siblings without scanning the inbox.
--
-- The server caps the unverified messages an inbox keeps, and asks, on every
-- unverified delivery, whether the cap is reached. This index holds only the
-- unverified rows, so that answer reads at most as many entries as the cap and
-- costs the same however many verified messages the inbox holds. A database with
-- no unverified messages adds nothing to the index.
CREATE INDEX messages_unverified ON messages(inbox_id, seq) WHERE verified = 0;
