-- A token may expire, and a token's last use is noted. Both are nullable: every token
-- that exists now has no expiry and has not been seen in use, which is what NULL means.
-- Times are fixed-width UTC like created_at, so they compare as text.
ALTER TABLE tokens ADD COLUMN expires_at   TEXT;
ALTER TABLE tokens ADD COLUMN last_used_at TEXT;
