-- Phase 7: new messages and forwards written in the web interface. Unlike
-- reply drafts, their recipients are typed by Louis. One row per message,
-- rewritten in place at each save (version + 1).
CREATE TABLE compositions (
    id               INTEGER PRIMARY KEY,
    kind             TEXT NOT NULL CHECK (kind IN ('new', 'forward')),
    source_pk        INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    version          INTEGER NOT NULL DEFAULT 1,
    to_json          TEXT NOT NULL DEFAULT '[]',
    cc_json          TEXT NOT NULL DEFAULT '[]',
    subject          TEXT NOT NULL DEFAULT '',
    body             TEXT NOT NULL DEFAULT '',
    rfc_message_id   TEXT NOT NULL DEFAULT '',
    raw              BLOB NOT NULL DEFAULT x'',
    imap_folder      TEXT NOT NULL DEFAULT '',
    imap_uidvalidity INTEGER NOT NULL DEFAULT 0,
    imap_uid         INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled', 'sending', 'sent')),
    error            TEXT NOT NULL DEFAULT '',
    sent_at          TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX compositions_status ON compositions(status, sent_at);
