package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SearchOptions is a classic search of the web interface: words (full-text,
// prefixes allowed), a sender and a date range. Every part is optional but
// at least one must be set.
type SearchOptions struct {
	AccountID int64
	Words     string    // free text, split by SearchTerms (no FTS5 syntax survives)
	From      string    // words of the sender's name or address
	After     time.Time // received at or after (zero: no bound)
	Before    time.Time // received before (zero: no bound)
	Limit     int
}

// Empty reports whether the search has nothing to look for.
func (o SearchOptions) Empty() bool {
	return len(SearchTerms(o.Words)) == 0 && len(SearchTerms(o.From)) == 0 && o.After.IsZero() && o.Before.IsZero()
}

// SearchMessages returns the stored messages matching every given part,
// newest first, in any folder (mails Louis archived are found too).
func (s *Store) SearchMessages(ctx context.Context, o SearchOptions) ([]ListItem, error) {
	if o.Empty() {
		return nil, nil
	}
	if o.Limit <= 0 || o.Limit > 200 {
		o.Limit = 100
	}
	if o.AccountID == 0 {
		o.AccountID = 1
	}
	where := []string{"m.account_id = ?"}
	args := []any{o.AccountID}
	// Full-text parts: every word must match (AND), each one as a quoted
	// prefix; the sender part only in the sender column (name and address,
	// case and accent insensitive).
	var match []string
	if terms := SearchTerms(o.Words); len(terms) > 0 {
		match = append(match, "("+ftsAll(terms)+")")
	}
	if terms := SearchTerms(o.From); len(terms) > 0 {
		match = append(match, "sender : ("+ftsAll(terms)+")")
	}
	if len(match) > 0 {
		where = append(where, "m.id IN (SELECT rowid FROM search_fts WHERE search_fts MATCH ?)")
		args = append(args, strings.Join(match, " AND "))
	}
	if !o.After.IsZero() {
		where = append(where, "m.received_at >= ?")
		args = append(args, ts(o.After))
	}
	if !o.Before.IsZero() {
		where = append(where, "m.received_at < ?")
		args = append(args, ts(o.Before))
	}
	args = append(args, o.Limit)
	// where holds only the fixed conditions above; every value is a parameter.
	q := "SELECT " + listCols + " FROM messages m " + latestAnalysisJoin + " WHERE " + strings.Join(where, " AND ") + //nolint:gosec // fixed SQL fragments only
		" ORDER BY m.received_at DESC, m.id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	return scanList(rows)
}

func ftsAll(terms []string) string {
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"*`
	}
	return strings.Join(parts, " AND ")
}

// Reminder is a reminder still waiting on Louis (proposed, or failed to
// reach Todoist), or one in Todoist due soon, with its mail.
type Reminder struct {
	ID        int64
	MessagePK int64
	Text      string
	DueDate   string // YYYY-MM-DD or ""
	DueTime   string
	Status    string
	From      Address
	Subject   string
}

// PendingReminders returns the reminders to decide (any date) and the created
// ones due on or before dueBy (YYYY-MM-DD), soonest first, from mails
// still in the mailbox and not suspect.
func (s *Store) PendingReminders(ctx context.Context, dueBy string, limit int) ([]Reminder, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT x.id, x.message_pk, x.text, x.due_date, x.due_time, x.status,
		m.from_addr, m.from_name, m.subject
		FROM actions x JOIN messages m ON m.id = x.message_pk `+latestAnalysisJoin+`
		WHERE m.absent = 0 AND COALESCE(a.ai_instructions, 0) = 0 AND COALESCE(a.category, '') != 'spam_probable'
		AND (x.status IN ('proposed', 'failed') OR (x.status = 'created' AND x.due_date != '' AND x.due_date <= ?))
		ORDER BY x.due_date = '', x.due_date, x.due_time = '', x.due_time, x.id LIMIT ?`, dueBy, limit)
	if err != nil {
		return nil, fmt.Errorf("open actions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Reminder
	for rows.Next() {
		var a Reminder
		if err := rows.Scan(&a.ID, &a.MessagePK, &a.Text, &a.DueDate, &a.DueTime, &a.Status,
			&a.From.Email, &a.From.Name, &a.Subject); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReceivedSince returns the inbox messages received at or after since,
// newest first (the « Aujourd'hui » page).
func (s *Store) ReceivedSince(ctx context.Context, mailbox string, since time.Time, limit int) ([]ListItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+listCols+` FROM messages m `+latestAnalysisJoin+`
		WHERE m.account_id = 1 AND m.mailbox = ? AND m.absent = 0 AND m.received_at >= ?
		ORDER BY m.received_at DESC, m.id DESC LIMIT ?`, mailbox, ts(since), limit)
	if err != nil {
		return nil, fmt.Errorf("received since: %w", err)
	}
	return scanList(rows)
}
