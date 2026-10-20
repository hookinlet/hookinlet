-- Named readers: how far a reader that gave itself a name says it has read an inbox,
-- so that its lag can be shown. A row is set only by an explicit acknowledgement,
-- never by reading.
--
-- epoch: the inbox epoch the position belongs to. Sequence numbers mean something only
-- within one epoch, so every row is deleted when an inbox gets a new epoch (RenewEpochs,
-- and the snapshot Backup writes), and one whose epoch is not the inbox's is never read.
-- Deleting an inbox deletes its readers.
CREATE TABLE readers (
  inbox_id   INTEGER NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  seq        INTEGER NOT NULL,
  epoch      TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,  -- fixed-width UTC, microseconds
  PRIMARY KEY (inbox_id, name)
) WITHOUT ROWID;
