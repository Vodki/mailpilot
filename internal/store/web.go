package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Views are the AI-sorted lists of the web interface. Each one is a fixed
// SQL condition on the latest successful analysis (alias a) of message m.
var Views = []string{ViewInbox, ViewUrgent, ViewTodo, ViewReply, ViewBills, ViewNewsletters, ViewSuspect}

// View names.
const (
	ViewInbox       = "inbox"
	ViewUrgent      = "urgent"
	ViewTodo        = "todo"
	ViewReply       = "reply"
	ViewBills       = "bills"
	ViewNewsletters = "newsletters"
	ViewSuspect     = "suspect"
)

const suspectCond = `(COALESCE(a.ai_instructions, 0) = 1 OR a.category = 'spam_probable')`

var viewCond = map[string]string{
	ViewInbox:  "1 = 1",
	ViewUrgent: "a.priority IN ('urgent', 'haute') AND NOT " + suspectCond,
	ViewTodo: "NOT " + suspectCond + ` AND EXISTS (SELECT 1 FROM actions x WHERE x.message_pk = m.id
		AND x.status IN ('proposed', 'creating', 'created', 'failed'))`,
	ViewReply:       "json_extract(a.result_json, '$.needs_reply') = 1 AND NOT " + suspectCond,
	ViewBills:       "a.category = 'facture_paiement'",
	ViewNewsletters: "a.category IN ('newsletter', 'notification_auto')",
	ViewSuspect:     suspectCond,
}

// latestAnalysisJoin attaches the latest successful analysis, if any.
const latestAnalysisJoin = `LEFT JOIN analyses a ON a.id = (SELECT id FROM analyses
	WHERE message_pk = m.id AND status = 'ok' ORDER BY id DESC LIMIT 1)`

// ListItem is one row of a web list: the message headers, its analysis
// and counters. No body.
type ListItem struct {
	ID             int64
	Mailbox        string
	UIDValidity    uint32
	UID            uint32
	From           Address
	Subject        string
	ReceivedAt     time.Time
	HasAttachments bool
	IsDirect       bool
	State          string

	Analyzed       bool
	Priority       string
	Category       string
	Summary        string
	AIInstructions bool
	NeedsReply     bool

	TasksCreated  int // actions sent to Todoist
	TasksProposed int // actions waiting for confirmation
	HasDraft      bool
}

// ListOptions filters ListMessages.
type ListOptions struct {
	AccountID int64
	Mailbox   string
	View      string
	Limit     int
	BeforeID  int64 // keyset pagination: rows with id < BeforeID (0 = from the newest)
}

// ErrUnknownView is returned for a view name not in Views.
var ErrUnknownView = errors.New("store: unknown view")

// ListMessages returns the messages of a view, newest first.
func (s *Store) ListMessages(ctx context.Context, o ListOptions) ([]ListItem, error) {
	cond, ok := viewCond[o.View]
	if !ok {
		return nil, ErrUnknownView
	}
	if o.Limit <= 0 || o.Limit > 500 {
		o.Limit = 100
	}
	if o.AccountID == 0 {
		o.AccountID = 1
	}
	q := `SELECT m.id, m.mailbox, m.uidvalidity, m.uid, m.from_addr, m.from_name, m.subject, m.received_at,
		m.has_attachments, m.is_direct, m.state,
		a.id IS NOT NULL, COALESCE(a.priority, ''), COALESCE(a.category, ''), COALESCE(a.summary, ''),
		COALESCE(a.ai_instructions, 0), COALESCE(json_extract(a.result_json, '$.needs_reply'), 0),
		(SELECT count(*) FROM actions x WHERE x.message_pk = m.id AND x.status = 'created'),
		(SELECT count(*) FROM actions x WHERE x.message_pk = m.id AND x.status IN ('proposed', 'failed')),
		EXISTS (SELECT 1 FROM drafts d WHERE d.message_pk = m.id AND d.status = 'active')
		FROM messages m ` + latestAnalysisJoin + `
		WHERE m.account_id = ? AND m.mailbox = ? AND m.absent = 0 AND (? = 0 OR m.id < ?) AND ` + cond + `
		ORDER BY m.received_at DESC, m.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, o.AccountID, o.Mailbox, o.BeforeID, o.BeforeID, o.Limit)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ListItem
	for rows.Next() {
		var it ListItem
		var recv string
		var att, direct, analyzed, ai, reply, draft int
		if err := rows.Scan(&it.ID, &it.Mailbox, &it.UIDValidity, &it.UID, &it.From.Email, &it.From.Name, &it.Subject, &recv,
			&att, &direct, &it.State, &analyzed, &it.Priority, &it.Category, &it.Summary, &ai, &reply,
			&it.TasksCreated, &it.TasksProposed, &draft); err != nil {
			return nil, err
		}
		it.ReceivedAt = parseTS(recv)
		it.HasAttachments, it.IsDirect, it.Analyzed = att == 1, direct == 1, analyzed == 1
		it.AIInstructions, it.NeedsReply, it.HasDraft = ai == 1, reply == 1, draft == 1
		out = append(out, it)
	}
	return out, rows.Err()
}

// ViewCounts returns the number of messages in each view.
func (s *Store) ViewCounts(ctx context.Context, accountID int64, mailbox string) (map[string]int, error) {
	if accountID == 0 {
		accountID = 1
	}
	out := map[string]int{}
	for _, v := range Views {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages m `+latestAnalysisJoin+`
			WHERE m.account_id = ? AND m.mailbox = ? AND m.absent = 0 AND `+viewCond[v], accountID, mailbox).Scan(&n)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", v, err)
		}
		out[v] = n
	}
	return out, nil
}

// ChangeVersion is a cheap fingerprint of what the web lists show: it
// changes when a message, an analysis, an action or a draft changes.
func (s *Store) ChangeVersion(ctx context.Context) (string, error) {
	var a, b, c, d sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT max(updated_at) || '-' || sum(absent) FROM messages), (SELECT max(id) FROM analyses),
		(SELECT max(updated_at) FROM actions), (SELECT max(id) || '-' || count(*) FROM drafts WHERE status = 'active')`).
		Scan(&a, &b, &c, &d)
	if err != nil {
		return "", err
	}
	return a.String + "|" + b.String + "|" + c.String + "|" + d.String, nil
}

// Account is a monitored mailbox.
type Account struct {
	ID      int64
	Name    string
	Address string
}

// EnsureAccount records the address of account id (created by migration 6
// for id 1).
func (s *Store) EnsureAccount(ctx context.Context, id int64, address string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts(id, name, address) VALUES(?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET address = excluded.address`, id, "principal", address)
	return err
}

// Accounts lists the accounts.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, address FROM accounts ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Address); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// WebSession is a logged-in browser.
type WebSession struct {
	TokenHash string
	CSRF      string
	ExpiresAt time.Time
	LastSeen  time.Time
}

// CreateWebSession stores a session by the hash of its token.
func (s *Store) CreateWebSession(ctx context.Context, ws WebSession, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions(token_hash, csrf, created_at, expires_at, last_seen)
		VALUES(?, ?, ?, ?, ?)`, ws.TokenHash, ws.CSRF, ts(now), ts(ws.ExpiresAt), ts(now))
	return err
}

// GetWebSession returns a live session; expired or unknown: ErrNotFound.
func (s *Store) GetWebSession(ctx context.Context, tokenHash string, now time.Time) (*WebSession, error) {
	var ws WebSession
	var exp, seen string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, csrf, expires_at, last_seen FROM web_sessions
		WHERE token_hash = ? AND expires_at > ?`, tokenHash, ts(now)).Scan(&ws.TokenHash, &ws.CSRF, &exp, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ws.ExpiresAt, ws.LastSeen = parseTS(exp), parseTS(seen)
	return &ws, nil
}

// TouchWebSession records activity (at most once a minute is enough).
func (s *Store) TouchWebSession(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE web_sessions SET last_seen = ? WHERE token_hash = ?", ts(now), tokenHash)
	return err
}

// DeleteWebSession logs a session out.
func (s *Store) DeleteWebSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE token_hash = ?", tokenHash)
	return err
}

// PurgeWebSessions removes expired sessions.
func (s *Store) PurgeWebSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE expires_at <= ?", ts(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
