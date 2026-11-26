package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Action statuses.
const (
	ActionProposed  = "proposed"
	ActionCreating  = "creating"
	ActionCreated   = "created"
	ActionDismissed = "dismissed"
	ActionFailed    = "failed"
)

// Action is a task extracted from a mail.
type Action struct {
	ID            int64
	MessagePK     int64
	Idx           int
	Text          string
	DueDate       string
	DueTime       string
	QuoteVerified bool
	Priority      string
	Status        string
	TodoistID     string
	Error         string
	CreatedAt     time.Time
}

// InsertAction stores an action once per (message, idx); returns the row id
// and whether it was new.
func (s *Store) InsertAction(ctx context.Context, a *Action) (int64, bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO actions(message_pk, idx, text, due_date, due_time,
		quote_verified, priority, status) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		a.MessagePK, a.Idx, a.Text, a.DueDate, a.DueTime, b2i(a.QuoteVerified), a.Priority, a.Status)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		id, _ := res.LastInsertId()
		a.ID = id
		return id, true, nil
	}
	err = s.db.QueryRowContext(ctx, "SELECT id FROM actions WHERE message_pk = ? AND idx = ?", a.MessagePK, a.Idx).Scan(&a.ID)
	return a.ID, false, err
}

const actionCols = `id, message_pk, idx, text, due_date, due_time, quote_verified, priority, status, todoist_id, error, created_at`

func scanAction(row interface{ Scan(...any) error }) (*Action, error) {
	var a Action
	var qv int
	var created string
	if err := row.Scan(&a.ID, &a.MessagePK, &a.Idx, &a.Text, &a.DueDate, &a.DueTime, &qv, &a.Priority, &a.Status,
		&a.TodoistID, &a.Error, &created); err != nil {
		return nil, err
	}
	a.QuoteVerified, a.CreatedAt = qv == 1, parseTS(created)
	return &a, nil
}

// GetAction loads one action.
func (s *Store) GetAction(ctx context.Context, id int64) (*Action, error) {
	a, err := scanAction(s.db.QueryRowContext(ctx, "SELECT "+actionCols+" FROM actions WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// MessageActions lists the actions of a message.
func (s *Store) MessageActions(ctx context.Context, messagePK int64) ([]*Action, error) {
	return s.queryActions(ctx, "SELECT "+actionCols+" FROM actions WHERE message_pk = ? ORDER BY idx", messagePK)
}

// OpenActions lists proposed or failed actions created since t.
func (s *Store) OpenActions(ctx context.Context, since time.Time, limit int) ([]*Action, error) {
	return s.queryActions(ctx, "SELECT "+actionCols+` FROM actions WHERE status IN ('proposed', 'failed')
		AND created_at >= ? ORDER BY id DESC LIMIT ?`, ts(since), limit)
}

func (s *Store) queryActions(ctx context.Context, q string, args ...any) ([]*Action, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClaimAction moves an action from one of the from statuses to "creating".
// It returns false when another path already claimed it (double click).
func (s *Store) ClaimAction(ctx context.Context, id int64, from ...string) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("no source status")
	}
	q := "UPDATE actions SET status = 'creating', updated_at = ? WHERE id = ? AND status IN (?" + strings.Repeat(", ?", len(from)-1) + ")" //nolint:gosec // only placeholders are concatenated
	args := []any{ts(time.Now()), id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetActionStatus records the outcome of an action.
func (s *Store) SetActionStatus(ctx context.Context, id int64, status, todoistID, errMsg string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE actions SET status = ?, todoist_id = ?, error = ?, updated_at = ? WHERE id = ?",
		status, todoistID, errMsg, ts(time.Now()), id)
	return err
}

// RecordRuleHits stores which rules matched a message.
func (s *Store) RecordRuleHits(ctx context.Context, messagePK int64, names []string) error {
	for _, n := range names {
		if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO rule_hits(rule, message_pk) VALUES(?, ?)", n, messagePK); err != nil {
			return err
		}
	}
	return nil
}

// RuleHitCounts returns hits per rule since t.
func (s *Store) RuleHitCounts(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT rule, COUNT(*) FROM rule_hits WHERE created_at >= ? GROUP BY rule", ts(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var r string
		var n int
		if err := rows.Scan(&r, &n); err != nil {
			return nil, err
		}
		out[r] = n
	}
	return out, rows.Err()
}

// Mute is an address or domain sent to the digest only.
type Mute struct {
	Pattern string
	Until   time.Time // zero = forever
}

// AddMute upserts a mute.
func (s *Store) AddMute(ctx context.Context, pattern string, until time.Time) error {
	var u any
	if !until.IsZero() {
		u = ts(until)
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO mutes(pattern, until) VALUES(?, ?) ON CONFLICT(pattern) DO UPDATE SET until = excluded.until",
		strings.ToLower(pattern), u)
	return err
}

// RemoveMute deletes a mute; false if it did not exist.
func (s *Store) RemoveMute(ctx context.Context, pattern string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM mutes WHERE pattern = ?", strings.ToLower(pattern))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Mutes lists the mutes active at now.
func (s *Store) Mutes(ctx context.Context, now time.Time) ([]Mute, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT pattern, COALESCE(until, '') FROM mutes WHERE until IS NULL OR until > ? ORDER BY pattern", ts(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Mute
	for rows.Next() {
		var m Mute
		var u string
		if err := rows.Scan(&m.Pattern, &u); err != nil {
			return nil, err
		}
		if u != "" {
			m.Until = parseTS(u)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
