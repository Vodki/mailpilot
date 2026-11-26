package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Relocate records where a message is after Louis moved it from the web
// interface (uid 0 when the server did not say: actions on it are then
// refused until the folder list sees it again).
func (s *Store) Relocate(ctx context.Context, id int64, mailbox string, uidValidity, uid uint32) error {
	res, err := s.db.ExecContext(ctx, `UPDATE OR IGNORE messages SET mailbox = ?, uidvalidity = ?, uid = ?, absent = 0, updated_at = ?
		WHERE id = ?`, mailbox, uidValidity, uid, ts(time.Now()), id)
	if err != nil {
		return fmt.Errorf("relocate: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SyncPresence flags absent the messages of a mailbox that are no longer
// there (moved or deleted from another client) and clears the flag of the
// ones that are. Only UIDs below uidNext are judged, so a message stored
// after the server was read is never hidden.
func (s *Store) SyncPresence(ctx context.Context, mailbox string, uidValidity, uidNext uint32, present []uint32) (int64, error) {
	if present == nil {
		present = []uint32{}
	}
	js, _ := json.Marshal(present)
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET
		absent = CASE WHEN uid IN (SELECT value FROM json_each(?1)) THEN 0 ELSE 1 END, updated_at = ?2
		WHERE mailbox = ?3 AND uidvalidity = ?4 AND uid > 0 AND uid < ?5
		AND absent != CASE WHEN uid IN (SELECT value FROM json_each(?1)) THEN 0 ELSE 1 END`,
		string(js), ts(time.Now()), mailbox, uidValidity, uidNext)
	if err != nil {
		return 0, fmt.Errorf("sync presence: %w", err)
	}
	return res.RowsAffected()
}

// Known is what the store holds about a message seen in a folder listing.
type Known struct {
	ID          int64
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	Analyzed    bool
	Priority    string
	Category    string
	Summary     string
	Suspect     bool
}

// KnownByMessageID returns the stored messages among these Message-IDs
// (without angle brackets), keyed by Message-ID.
func (s *Store) KnownByMessageID(ctx context.Context, ids []string) (map[string]Known, error) {
	out := map[string]Known{}
	if len(ids) == 0 {
		return out, nil
	}
	js, _ := json.Marshal(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT m.message_id, m.id, m.mailbox, m.uidvalidity, m.uid,
		a.id IS NOT NULL, COALESCE(a.priority, ''), COALESCE(a.category, ''), COALESCE(a.summary, ''),
		COALESCE(a.ai_instructions, 0) = 1 OR COALESCE(a.category, '') = 'spam_probable'
		FROM messages m `+latestAnalysisJoin+`
		WHERE m.message_id IN (SELECT value FROM json_each(?))`, string(js))
	if err != nil {
		return nil, fmt.Errorf("known: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mid string
		var k Known
		var analyzed, suspect int
		if err := rows.Scan(&mid, &k.ID, &k.Mailbox, &k.UIDValidity, &k.UID, &analyzed, &k.Priority, &k.Category, &k.Summary, &suspect); err != nil {
			return nil, err
		}
		k.Analyzed, k.Suspect = analyzed == 1, suspect == 1
		out[mid] = k
	}
	return out, rows.Err()
}

// IDAt returns the stored message at this exact location.
func (s *Store) IDAt(ctx context.Context, mailbox string, uidValidity, uid uint32) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM messages WHERE mailbox = ? AND uidvalidity = ? AND uid = ? AND uid > 0",
		mailbox, uidValidity, uid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
