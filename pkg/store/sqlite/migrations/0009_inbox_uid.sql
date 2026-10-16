-- uid: an identifier for one inbox that never changes and is never handed to
-- another inbox. A verifier secret is sealed to it (inbox.VerifierSecretContext)
-- where it used to be sealed to the slug, which a later inbox can have again.
--
-- Existing inboxes get a random uid. A secret already stored was sealed to the
-- slug, which the migration cannot redo (it does not have the master key), so
-- such a secret no longer opens: the server names those inboxes and refuses to
-- start, and `hookinlet inbox set-secret SLUG` sets each again.
ALTER TABLE inboxes ADD COLUMN uid TEXT NOT NULL DEFAULT '';
UPDATE inboxes SET uid = lower(hex(randomblob(16)));
CREATE UNIQUE INDEX inboxes_uid ON inboxes(uid);
