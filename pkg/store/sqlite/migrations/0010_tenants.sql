-- tenant: which independent party an inbox belongs to (see inbox.Inbox.Tenant).
-- Every existing inbox is in the default tenant, the empty one.
--
-- A slug is unique within a tenant and not across them, which the column that
-- held it could not be: it is UNIQUE, and SQLite cannot drop a constraint from a
-- column without rebuilding the table, which with the foreign keys that point at
-- it would delete every message. So the old column is renamed and the slug goes
-- in a new one. The old column is not used any more; a new inbox gets its uid in
-- it, which keeps it unique, and it is never read.
ALTER TABLE inboxes ADD COLUMN tenant TEXT NOT NULL DEFAULT '';
ALTER TABLE inboxes RENAME COLUMN slug TO slug_unused;
ALTER TABLE inboxes ADD COLUMN slug TEXT NOT NULL DEFAULT '';
UPDATE inboxes SET slug = slug_unused, slug_unused = uid;
CREATE UNIQUE INDEX inboxes_tenant_slug ON inboxes(tenant, slug);
