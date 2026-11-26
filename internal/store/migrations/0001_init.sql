-- Phase 0: process state and audit trail. Mail tables arrive in phase 1.

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

CREATE TABLE mailbox_state (
    mailbox        TEXT PRIMARY KEY,
    uidvalidity    INTEGER NOT NULL,
    last_uid       INTEGER NOT NULL DEFAULT 0,
    last_ok_at     TEXT,
    bootstrap_done INTEGER NOT NULL DEFAULT 0 CHECK (bootstrap_done IN (0, 1))
) STRICT;

-- Security-relevant events: rejected Telegram senders, /pause, sends.
-- ref holds identifiers only, never message content.
CREATE TABLE audit_log (
    id    INTEGER PRIMARY KEY,
    ts    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor TEXT NOT NULL,
    event TEXT NOT NULL,
    ref   TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX audit_log_ts ON audit_log(ts);
