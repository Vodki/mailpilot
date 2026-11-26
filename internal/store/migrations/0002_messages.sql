-- Phase 1: messages, analyses, notifications, digests, callbacks.

CREATE TABLE threads (
    id              INTEGER PRIMARY KEY,
    root_message_id TEXT NOT NULL UNIQUE,
    subject_norm    TEXT NOT NULL DEFAULT '',
    last_at         TEXT NOT NULL
) STRICT;

CREATE TABLE messages (
    id              INTEGER PRIMARY KEY,
    message_id      TEXT NOT NULL UNIQUE,
    synthetic_id    INTEGER NOT NULL DEFAULT 0,
    mailbox         TEXT NOT NULL,
    uidvalidity     INTEGER NOT NULL,
    uid             INTEGER NOT NULL,
    thread_id       INTEGER REFERENCES threads(id),
    from_addr       TEXT NOT NULL DEFAULT '',
    from_name       TEXT NOT NULL DEFAULT '',
    reply_to_json   TEXT NOT NULL DEFAULT '[]',
    to_json         TEXT NOT NULL DEFAULT '[]',
    cc_json         TEXT NOT NULL DEFAULT '[]',
    subject         TEXT NOT NULL DEFAULT '',
    sent_at         TEXT,
    received_at     TEXT NOT NULL,
    in_reply_to     TEXT NOT NULL DEFAULT '',
    refs_json       TEXT NOT NULL DEFAULT '[]',
    list_id         TEXT NOT NULL DEFAULT '',
    auto_submitted  INTEGER NOT NULL DEFAULT 0,
    has_attachments INTEGER NOT NULL DEFAULT 0,
    is_direct       INTEGER NOT NULL DEFAULT 0,
    size            INTEGER NOT NULL DEFAULT 0,
    body_sha256     TEXT NOT NULL DEFAULT '',
    -- new -> analyzed | analysis_failed | skipped ; see pipeline package
    state           TEXT NOT NULL DEFAULT 'new',
    -- pending -> immediate | digest | silent | ignored
    delivery        TEXT NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (mailbox, uidvalidity, uid)
) STRICT;

CREATE INDEX messages_state ON messages(state, delivery);
CREATE INDEX messages_thread ON messages(thread_id);
CREATE INDEX messages_received ON messages(received_at);

CREATE TABLE message_bodies (
    message_pk INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    text_clean TEXT NOT NULL DEFAULT '',
    text_full  TEXT NOT NULL DEFAULT '',
    html_only  INTEGER NOT NULL DEFAULT 0,
    purged_at  TEXT
) STRICT;

CREATE TABLE attachments (
    id         INTEGER PRIMARY KEY,
    message_pk INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    filename   TEXT NOT NULL,
    mime_type  TEXT NOT NULL,
    size       INTEGER NOT NULL,
    inline     INTEGER NOT NULL DEFAULT 0,
    pdf_text   TEXT
) STRICT;

CREATE INDEX attachments_message ON attachments(message_pk);

CREATE TABLE analyses (
    id              INTEGER PRIMARY KEY,
    message_pk      INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    attempt         INTEGER NOT NULL,
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL DEFAULT '',
    prompt_hash     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('ok', 'invalid', 'error')),
    category        TEXT NOT NULL DEFAULT '',
    priority        TEXT NOT NULL DEFAULT '',
    summary         TEXT NOT NULL DEFAULT '',
    result_json     TEXT NOT NULL DEFAULT '{}',
    ai_instructions INTEGER NOT NULL DEFAULT 0,
    error           TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX analyses_message ON analyses(message_pk, id);

CREATE TABLE llm_calls (
    id              INTEGER PRIMARY KEY,
    task            TEXT NOT NULL,
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL DEFAULT '',
    message_pk      INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_cached   INTEGER NOT NULL DEFAULT 0,
    cost_usd_micros INTEGER NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    outcome         TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX llm_calls_created ON llm_calls(created_at);

CREATE TABLE digests (
    id      INTEGER PRIMARY KEY,
    slot    TEXT NOT NULL UNIQUE,
    sent_at TEXT
) STRICT;

CREATE TABLE notifications (
    id            INTEGER PRIMARY KEY,
    message_pk    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    tg_message_id INTEGER NOT NULL DEFAULT 0,
    digest_id     INTEGER REFERENCES digests(id),
    sent_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (message_pk, kind)
) STRICT;

-- Telegram callback_data holds only an opaque random token (64-byte limit).
CREATE TABLE callbacks (
    token      TEXT PRIMARY KEY,
    action     TEXT NOT NULL,
    message_pk INTEGER REFERENCES messages(id) ON DELETE CASCADE,
    draft_pk   INTEGER,
    arg        TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;
