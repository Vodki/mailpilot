package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Draft statuses.
const (
	DraftActive     = "active"
	DraftSuperseded = "superseded"
	DraftCancelled  = "cancelled"
	DraftSending    = "sending"
	DraftSent       = "sent"
	DraftFailed     = "failed"
)

// Draft is one version of a reply.
type Draft struct {
	ID              int64
	MessagePK       int64
	Version         int
	Body            string
	Instructions    string
	ReplyAll        bool
	To, Cc          []Address
	Subject         string
	RFCMessageID    string
	Raw             []byte // exact message stored in Drafts and sent
	IMAPFolder      string
	IMAPUIDValidity uint32
	IMAPUID         uint32
	TGMessageID     int64
	Status          string
	Error           string
	SentAt          time.Time
	CreatedAt       time.Time
}

// NewDraftVersion supersedes the active draft of the message (if any) and
// inserts the next version as active, atomically. It returns the previous
// active draft so its IMAP copy and Telegram buttons can be cleaned up.
func (s *Store) NewDraftVersion(ctx context.Context, d *Draft) (prev *Draft, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	prev, err = scanDraft(tx.QueryRowContext(ctx, "SELECT "+draftCols+" FROM drafts WHERE message_pk = ? AND status = 'active'", d.MessagePK))
	if errors.Is(err, sql.ErrNoRows) {
		prev, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	if prev != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE drafts SET status = 'superseded' WHERE id = ?", prev.ID); err != nil {
			return nil, err
		}
	}
	var v int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) + 1 FROM drafts WHERE message_pk = ?", d.MessagePK).Scan(&v); err != nil {
		return nil, err
	}
	d.Version, d.Status = v, DraftActive
	js := func(x []Address) string {
		if x == nil {
			x = []Address{}
		}
		b, _ := json.Marshal(x)
		return string(b)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO drafts(message_pk, version, body, instructions, reply_all, to_json, cc_json,
		subject, rfc_message_id, raw, status) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active')`,
		d.MessagePK, v, d.Body, d.Instructions, b2i(d.ReplyAll), js(d.To), js(d.Cc), d.Subject, d.RFCMessageID, d.Raw)
	if err != nil {
		return nil, err
	}
	d.ID, _ = res.LastInsertId()
	return prev, tx.Commit()
}

const draftCols = `id, message_pk, version, body, instructions, reply_all, to_json, cc_json, subject, rfc_message_id, raw,
	imap_folder, imap_uidvalidity, imap_uid, tg_message_id, status, error, COALESCE(sent_at, ''), created_at`

func scanDraft(row interface{ Scan(...any) error }) (*Draft, error) {
	var d Draft
	var ra int
	var to, cc, sent, created string
	if err := row.Scan(&d.ID, &d.MessagePK, &d.Version, &d.Body, &d.Instructions, &ra, &to, &cc, &d.Subject, &d.RFCMessageID, &d.Raw,
		&d.IMAPFolder, &d.IMAPUIDValidity, &d.IMAPUID, &d.TGMessageID, &d.Status, &d.Error, &sent, &created); err != nil {
		return nil, err
	}
	d.ReplyAll = ra == 1
	_ = json.Unmarshal([]byte(to), &d.To)
	_ = json.Unmarshal([]byte(cc), &d.Cc)
	if sent != "" {
		d.SentAt = parseTS(sent)
	}
	d.CreatedAt = parseTS(created)
	return &d, nil
}

// GetDraft loads a draft.
func (s *Store) GetDraft(ctx context.Context, id int64) (*Draft, error) {
	d, err := scanDraft(s.db.QueryRowContext(ctx, "SELECT "+draftCols+" FROM drafts WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// ActiveDraft returns the active draft of a message.
func (s *Store) ActiveDraft(ctx context.Context, messagePK int64) (*Draft, error) {
	d, err := scanDraft(s.db.QueryRowContext(ctx, "SELECT "+draftCols+" FROM drafts WHERE message_pk = ? AND status = 'active'", messagePK))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// SetDraftIMAP records where the draft was appended.
func (s *Store) SetDraftIMAP(ctx context.Context, id int64, folder string, uidValidity, uid uint32) error {
	_, err := s.db.ExecContext(ctx, "UPDATE drafts SET imap_folder = ?, imap_uidvalidity = ?, imap_uid = ? WHERE id = ?", folder, uidValidity, uid, id)
	return err
}

// SetDraftTelegram records the Telegram message that shows the draft.
func (s *Store) SetDraftTelegram(ctx context.Context, id, tgID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE drafts SET tg_message_id = ? WHERE id = ?", tgID, id)
	return err
}

// TransitionDraft moves a draft from one status to another; false if it
// was not in the expected status (double click, superseded meanwhile).
// at is the caller's clock, recorded as sent_at for DraftSent.
func (s *Store) TransitionDraft(ctx context.Context, id int64, from, to, errMsg string, at time.Time) (bool, error) {
	var sent any
	if to == DraftSent {
		sent = ts(at)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE drafts SET status = ?, error = ?, sent_at = COALESCE(?, sent_at) WHERE id = ? AND status = ?",
		to, errMsg, sent, id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SentSince counts drafts sent (or being sent) since t, for the rate limit.
func (s *Store) SentSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM drafts WHERE (status = 'sent' AND sent_at >= ?) OR status = 'sending'", ts(t)).Scan(&n)
	return n, err
}
