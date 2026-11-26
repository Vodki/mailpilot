-- Phase 3: reply drafts and sends.
-- status: active (current version) | superseded | cancelled | sending | sent | failed
CREATE TABLE drafts (
    id             INTEGER PRIMARY KEY,
    message_pk     INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    version        INTEGER NOT NULL,
    body           TEXT NOT NULL,
    instructions   TEXT NOT NULL DEFAULT '',
    reply_all      INTEGER NOT NULL DEFAULT 0,
    to_json        TEXT NOT NULL DEFAULT '[]',
    cc_json        TEXT NOT NULL DEFAULT '[]',
    subject        TEXT NOT NULL DEFAULT '',
    rfc_message_id TEXT NOT NULL DEFAULT '',
    raw            BLOB NOT NULL DEFAULT x'',
    imap_folder    TEXT NOT NULL DEFAULT '',
    imap_uidvalidity INTEGER NOT NULL DEFAULT 0,
    imap_uid       INTEGER NOT NULL DEFAULT 0,
    tg_message_id  INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active', 'superseded', 'cancelled', 'sending', 'sent', 'failed')),
    error          TEXT NOT NULL DEFAULT '',
    sent_at        TEXT,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (message_pk, version)
) STRICT;

CREATE INDEX drafts_status ON drafts(status, sent_at);
