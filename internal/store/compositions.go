package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Composition kinds.
const (
	CompositionNew     = "new"
	CompositionForward = "forward"
)

// Composition is a new message or a forward written in the web interface.
type Composition struct {
	ID              int64
	Kind            string
	SourcePK        int64 // forwarded message (0 for a new message)
	Version         int
	To, Cc          []Address
	Subject         string
	Body            string
	RFCMessageID    string
	Raw             []byte
	IMAPFolder      string
	IMAPUIDValidity uint32
	IMAPUID         uint32
	Status          string
	Error           string
	SentAt          time.Time
	UpdatedAt       time.Time
}

const compositionCols = `id, kind, COALESCE(source_pk, 0), version, to_json, cc_json, subject, body, rfc_message_id, raw,
	imap_folder, imap_uidvalidity, imap_uid, status, error, COALESCE(sent_at, ''), updated_at`

func scanComposition(row interface{ Scan(...any) error }) (*Composition, error) {
	var c Composition
	var to, cc, sent, upd string
	if err := row.Scan(&c.ID, &c.Kind, &c.SourcePK, &c.Version, &to, &cc, &c.Subject, &c.Body, &c.RFCMessageID, &c.Raw,
		&c.IMAPFolder, &c.IMAPUIDValidity, &c.IMAPUID, &c.Status, &c.Error, &sent, &upd); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(to), &c.To)
	_ = json.Unmarshal([]byte(cc), &c.Cc)
	if sent != "" {
		c.SentAt = parseTS(sent)
	}
	c.UpdatedAt = parseTS(upd)
	return &c, nil
}

// SaveComposition inserts a composition (ID 0, version 1) or rewrites an
// active one as its next version. It returns the previous state, whose
// IMAP copy the caller removes.
func (s *Store) SaveComposition(ctx context.Context, c *Composition) (prev *Composition, err error) {
	js := func(v []Address) string {
		if v == nil {
			v = []Address{}
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	var src any
	if c.SourcePK > 0 {
		src = c.SourcePK
	}
	now := ts(time.Now())
	if c.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO compositions(kind, source_pk, to_json, cc_json, subject, body, rfc_message_id, raw, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.Kind, src, js(c.To), js(c.Cc), c.Subject, c.Body, c.RFCMessageID, c.Raw, now)
		if err != nil {
			return nil, err
		}
		c.ID, _ = res.LastInsertId()
		c.Version, c.Status = 1, "active"
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	prev, err = scanComposition(tx.QueryRowContext(ctx, "SELECT "+compositionCols+" FROM compositions WHERE id = ? AND status = 'active'", c.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Version, c.Status, c.Kind, c.SourcePK = prev.Version+1, "active", prev.Kind, prev.SourcePK
	if _, err := tx.ExecContext(ctx, `UPDATE compositions SET version = ?, to_json = ?, cc_json = ?, subject = ?, body = ?,
		rfc_message_id = ?, raw = ?, imap_folder = '', imap_uidvalidity = 0, imap_uid = 0, updated_at = ? WHERE id = ?`,
		c.Version, js(c.To), js(c.Cc), c.Subject, c.Body, c.RFCMessageID, c.Raw, now, c.ID); err != nil {
		return nil, err
	}
	return prev, tx.Commit()
}

// GetComposition loads one composition.
func (s *Store) GetComposition(ctx context.Context, id int64) (*Composition, error) {
	c, err := scanComposition(s.db.QueryRowContext(ctx, "SELECT "+compositionCols+" FROM compositions WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ActiveCompositions lists the compositions not sent nor dropped, newest
// first.
func (s *Store) ActiveCompositions(ctx context.Context, limit int) ([]*Composition, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+compositionCols+" FROM compositions WHERE status = 'active' ORDER BY updated_at DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Composition
	for rows.Next() {
		c, err := scanComposition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCompositionIMAP records where the draft copy is stored.
func (s *Store) SetCompositionIMAP(ctx context.Context, id int64, folder string, uidValidity, uid uint32) error {
	_, err := s.db.ExecContext(ctx, "UPDATE compositions SET imap_folder = ?, imap_uidvalidity = ?, imap_uid = ? WHERE id = ?",
		folder, uidValidity, uid, id)
	return err
}

// TransitionComposition moves a composition from one status to another,
// atomically: only one caller wins (a double click sends once).
func (s *Store) TransitionComposition(ctx context.Context, id int64, from, to, errMsg string, at time.Time) (bool, error) {
	var sent any
	if to == "sent" {
		sent = ts(at)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE compositions SET status = ?, error = ?, sent_at = COALESCE(?, sent_at), updated_at = ?
		WHERE id = ? AND status = ?`, to, errMsg, sent, ts(at), id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
