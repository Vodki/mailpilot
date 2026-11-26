-- Phase 2: rule hits, extracted actions (Todoist), mutes.

CREATE TABLE rule_hits (
    id         INTEGER PRIMARY KEY,
    rule       TEXT NOT NULL,
    message_pk INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (rule, message_pk)
) STRICT;

-- One row per action Louis owns. status:
--   proposed  -> waiting for Louis (confirm mode)
--   creating  -> claimed, Todoist call in flight
--   created   -> task exists (todoist_id)
--   dismissed -> refused by Louis
--   failed    -> Todoist call failed (retried from /rappels)
CREATE TABLE actions (
    id             INTEGER PRIMARY KEY,
    message_pk     INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    idx            INTEGER NOT NULL,
    text           TEXT NOT NULL,
    due_date       TEXT NOT NULL DEFAULT '',
    due_time       TEXT NOT NULL DEFAULT '',
    quote_verified INTEGER NOT NULL DEFAULT 0,
    priority       TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL CHECK (status IN ('proposed', 'creating', 'created', 'dismissed', 'failed')),
    todoist_id     TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (message_pk, idx)
) STRICT;

CREATE INDEX actions_status ON actions(status, created_at);

-- /mute: an address or a domain whose mails go to the digest only.
CREATE TABLE mutes (
    pattern    TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    until      TEXT
) STRICT;
