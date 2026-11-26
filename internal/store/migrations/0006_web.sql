-- Phase 5: web interface. Accounts prepare several mailboxes (one for now:
-- id 1, the configured IMAP account); web sessions hold only token hashes.

CREATE TABLE accounts (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    address    TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

INSERT INTO accounts(id, name) VALUES (1, 'principal');

-- No REFERENCES: SQLite refuses a foreign key with a non-NULL default in
-- ALTER TABLE. Every message belongs to account 1 until multi-account lands.
ALTER TABLE messages ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;
CREATE INDEX messages_account_received ON messages(account_id, mailbox, received_at);

CREATE TABLE web_sessions (
    token_hash TEXT PRIMARY KEY,       -- sha256 of the cookie value
    csrf       TEXT NOT NULL,          -- per-session anti-CSRF token
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    last_seen  TEXT NOT NULL
) STRICT;

CREATE INDEX web_sessions_expires ON web_sessions(expires_at);
