-- Phase 4: full-text index for /ask. Kept in sync by triggers, so the index
-- follows inserts, analyses and the body purge (a purged body leaves the
-- index too; subject, sender and summary stay searchable).

CREATE VIRTUAL TABLE search_fts USING fts5(
    subject, sender, body, summary,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER search_msg_ai AFTER INSERT ON messages BEGIN
    INSERT INTO search_fts(rowid, subject, sender, body, summary)
    VALUES (new.id, new.subject, new.from_name || ' ' || new.from_addr, '', '');
END;

CREATE TRIGGER search_msg_ad AFTER DELETE ON messages BEGIN
    DELETE FROM search_fts WHERE rowid = old.id;
END;

CREATE TRIGGER search_body_ai AFTER INSERT ON message_bodies BEGIN
    UPDATE search_fts SET body = new.text_clean WHERE rowid = new.message_pk;
END;

CREATE TRIGGER search_body_au AFTER UPDATE OF text_clean ON message_bodies BEGIN
    UPDATE search_fts SET body = new.text_clean WHERE rowid = new.message_pk;
END;

CREATE TRIGGER search_analysis_ai AFTER INSERT ON analyses WHEN new.status = 'ok' BEGIN
    UPDATE search_fts SET summary = new.summary WHERE rowid = new.message_pk;
END;

INSERT INTO search_fts(rowid, subject, sender, body, summary)
SELECT m.id, m.subject, m.from_name || ' ' || m.from_addr, coalesce(b.text_clean, ''),
       coalesce((SELECT a.summary FROM analyses a WHERE a.message_pk = m.id AND a.status = 'ok' ORDER BY a.id DESC LIMIT 1), '')
FROM messages m LEFT JOIN message_bodies b ON b.message_pk = m.id;
