-- An ingest key is a capability, so it must not be a name anyone can guess.
-- Migration 3 gave every existing inbox its slug as its key, to keep their URLs
-- working. Nothing relied on that yet, so they get random keys now. Senders need
-- the new URL (hookinlet inbox show SLUG prints it). An inbox whose admin chose a
-- key equal to its slug is changed too: that is the guessable kind.
UPDATE inboxes SET ingest_key = lower(hex(randomblob(16))) WHERE ingest_key = slug;
