package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ts formats a time for storage (UTC, sortable).
func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func parseTS(s string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000Z", s)
	return t
}

// Message states and deliveries (see migrations/0002_messages.sql).
const (
	StateNew            = "new"
	StateAnalyzed       = "analyzed"
	StateAnalysisFailed = "analysis_failed"
	StateSkipped        = "skipped" // no LLM by rule or bootstrap

	DeliveryPending   = "pending"
	DeliveryImmediate = "immediate"
	DeliveryDigest    = "digest"
	DeliverySilent    = "silent"
	DeliveryIgnored   = "ignored"
)

// MailboxState is the IMAP synchronisation cursor.
type MailboxState struct {
	Mailbox       string
	UIDValidity   uint32
	LastUID       uint32
	LastOK        time.Time
	BootstrapDone bool
}

// GetMailboxState returns the cursor, ok=false if never synced.
func (s *Store) GetMailboxState(ctx context.Context, mailbox string) (MailboxState, bool, error) {
	var st MailboxState
	var lastOK sql.NullString
	var boot int
	err := s.db.QueryRowContext(ctx,
		"SELECT mailbox, uidvalidity, last_uid, last_ok_at, bootstrap_done FROM mailbox_state WHERE mailbox = ?", mailbox).
		Scan(&st.Mailbox, &st.UIDValidity, &st.LastUID, &lastOK, &boot)
	if errors.Is(err, sql.ErrNoRows) {
		return MailboxState{Mailbox: mailbox}, false, nil
	}
	if err != nil {
		return st, false, err
	}
	st.BootstrapDone = boot == 1
	if lastOK.Valid {
		st.LastOK = parseTS(lastOK.String)
	}
	return st, true, nil
}

// SaveMailboxState upserts the cursor.
func (s *Store) SaveMailboxState(ctx context.Context, st MailboxState) error {
	boot := 0
	if st.BootstrapDone {
		boot = 1
	}
	var lastOK any
	if !st.LastOK.IsZero() {
		lastOK = ts(st.LastOK)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailbox_state(mailbox, uidvalidity, last_uid, last_ok_at, bootstrap_done) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(mailbox) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = excluded.last_uid,
			last_ok_at = COALESCE(excluded.last_ok_at, last_ok_at), bootstrap_done = excluded.bootstrap_done`,
		st.Mailbox, st.UIDValidity, st.LastUID, lastOK, boot)
	return err
}

// Address mirrors parse.Address for storage without importing parse.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Attachment mirrors parse.Attachment.
type Attachment struct {
	Filename string
	MIMEType string
	Size     int64
	Inline   bool
}

// Message is a stored email.
type Message struct {
	ID             int64
	MessageID      string
	SyntheticID    bool
	Mailbox        string
	UIDValidity    uint32
	UID            uint32
	ThreadID       int64
	From           Address
	ReplyTo        []Address
	To             []Address
	Cc             []Address
	Subject        string
	SentAt         time.Time
	ReceivedAt     time.Time
	InReplyTo      string
	References     []string
	ListID         string
	AutoSubmitted  bool
	HasAttachments bool
	IsDirect       bool
	Size           int64
	BodySHA256     string
	State          string
	Delivery       string
	Attempts       int

	TextClean   string
	TextFull    string
	HTMLOnly    bool
	BodyPurged  bool
	Attachments []Attachment
}

// InsertMessage stores a new message, deduplicated on Message-ID. When the
// Message-ID is already known (restart, reconnect, UIDVALIDITY change) it
// only refreshes the IMAP locator and returns inserted=false.
func (s *Store) InsertMessage(ctx context.Context, m *Message, threadKeys []string, subjectNorm string) (id int64, inserted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM messages WHERE message_id = ?", m.MessageID).Scan(&existing)
	if err == nil {
		_, err = tx.ExecContext(ctx,
			"UPDATE OR IGNORE messages SET mailbox = ?, uidvalidity = ?, uid = ?, absent = 0, updated_at = ? WHERE id = ?",
			m.Mailbox, m.UIDValidity, m.UID, ts(time.Now()), existing)
		if err != nil {
			return 0, false, err
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	threadID, err := resolveThread(ctx, tx, m, threadKeys, subjectNorm)
	if err != nil {
		return 0, false, err
	}
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if m.ReplyTo == nil {
		m.ReplyTo = []Address{}
	}
	if m.To == nil {
		m.To = []Address{}
	}
	if m.Cc == nil {
		m.Cc = []Address{}
	}
	if m.References == nil {
		m.References = []string{}
	}
	var sent any
	if !m.SentAt.IsZero() {
		sent = ts(m.SentAt)
	}
	if m.State == "" {
		m.State = StateNew
	}
	if m.Delivery == "" {
		m.Delivery = DeliveryPending
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO messages(message_id, synthetic_id, mailbox, uidvalidity, uid, thread_id, from_addr, from_name,
			reply_to_json, to_json, cc_json, subject, sent_at, received_at, in_reply_to, refs_json, list_id,
			auto_submitted, has_attachments, is_direct, size, body_sha256, state, delivery)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.MessageID, b2i(m.SyntheticID), m.Mailbox, m.UIDValidity, m.UID, threadID, m.From.Email, m.From.Name,
		js(m.ReplyTo), js(m.To), js(m.Cc), m.Subject, sent, ts(m.ReceivedAt), m.InReplyTo, js(m.References), m.ListID,
		b2i(m.AutoSubmitted), b2i(m.HasAttachments), b2i(m.IsDirect), m.Size, m.BodySHA256, m.State, m.Delivery)
	if err != nil {
		return 0, false, fmt.Errorf("insert message: %w", err)
	}
	id, _ = res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO message_bodies(message_pk, text_clean, text_full, html_only) VALUES(?, ?, ?, ?)",
		id, m.TextClean, m.TextFull, b2i(m.HTMLOnly)); err != nil {
		return 0, false, err
	}
	for _, a := range m.Attachments {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO attachments(message_pk, filename, mime_type, size, inline) VALUES(?, ?, ?, ?, ?)",
			id, a.Filename, a.MIMEType, a.Size, b2i(a.Inline)); err != nil {
			return 0, false, err
		}
	}
	m.ID, m.ThreadID = id, threadID
	return id, true, tx.Commit()
}

// resolveThread finds the thread of any referenced message, else creates one
// rooted at the oldest reference (or the message itself).
func resolveThread(ctx context.Context, tx *sql.Tx, m *Message, keys []string, subjectNorm string) (int64, error) {
	now := ts(m.ReceivedAt)
	for _, k := range keys {
		var tid int64
		err := tx.QueryRowContext(ctx, "SELECT thread_id FROM messages WHERE message_id = ? AND thread_id IS NOT NULL", k).Scan(&tid)
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE threads SET last_at = max(last_at, ?) WHERE id = ?", now, tid)
			return tid, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		err = tx.QueryRowContext(ctx, "SELECT id FROM threads WHERE root_message_id = ?", k).Scan(&tid)
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE threads SET last_at = max(last_at, ?) WHERE id = ?", now, tid)
			return tid, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	root := m.MessageID
	if len(keys) > 0 {
		root = keys[0]
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO threads(root_message_id, subject_norm, last_at) VALUES(?, ?, ?) ON CONFLICT(root_message_id) DO UPDATE SET last_at = excluded.last_at",
		root, subjectNorm, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil || id == 0 {
		err = tx.QueryRowContext(ctx, "SELECT id FROM threads WHERE root_message_id = ?", root).Scan(&id)
	}
	return id, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

const messageCols = `m.id, m.message_id, m.synthetic_id, m.mailbox, m.uidvalidity, m.uid, COALESCE(m.thread_id, 0),
	m.from_addr, m.from_name, m.reply_to_json, m.to_json, m.cc_json, m.subject, COALESCE(m.sent_at, ''), m.received_at,
	m.in_reply_to, m.refs_json, m.list_id, m.auto_submitted, m.has_attachments, m.is_direct, m.size, m.body_sha256,
	m.state, m.delivery, m.attempts, COALESCE(b.text_clean, ''), COALESCE(b.text_full, ''), COALESCE(b.html_only, 0),
	b.purged_at IS NOT NULL`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var synth, auto, att, direct, html int
	var purged bool
	var replyTo, to, cc, refs, sent, recv string
	err := row.Scan(&m.ID, &m.MessageID, &synth, &m.Mailbox, &m.UIDValidity, &m.UID, &m.ThreadID,
		&m.From.Email, &m.From.Name, &replyTo, &to, &cc, &m.Subject, &sent, &recv,
		&m.InReplyTo, &refs, &m.ListID, &auto, &att, &direct, &m.Size, &m.BodySHA256,
		&m.State, &m.Delivery, &m.Attempts, &m.TextClean, &m.TextFull, &html, &purged)
	if err != nil {
		return nil, err
	}
	m.SyntheticID, m.AutoSubmitted, m.HasAttachments, m.IsDirect, m.HTMLOnly, m.BodyPurged = synth == 1, auto == 1, att == 1, direct == 1, html == 1, purged
	_ = json.Unmarshal([]byte(replyTo), &m.ReplyTo)
	_ = json.Unmarshal([]byte(to), &m.To)
	_ = json.Unmarshal([]byte(cc), &m.Cc)
	_ = json.Unmarshal([]byte(refs), &m.References)
	if sent != "" {
		m.SentAt = parseTS(sent)
	}
	m.ReceivedAt = parseTS(recv)
	return &m, nil
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// GetMessage loads a message with its body and attachments.
func (s *Store) GetMessage(ctx context.Context, id int64) (*Message, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+messageCols+" FROM messages m LEFT JOIN message_bodies b ON b.message_pk = m.id WHERE m.id = ?", id)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT filename, mime_type, size, inline FROM attachments WHERE message_pk = ? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a Attachment
		var inline int
		if err := rows.Scan(&a.Filename, &a.MIMEType, &a.Size, &inline); err != nil {
			return nil, err
		}
		a.Inline = inline == 1
		m.Attachments = append(m.Attachments, a)
	}
	return m, rows.Err()
}

// ThreadMessages returns the messages of a thread, oldest first.
func (s *Store) ThreadMessages(ctx context.Context, threadID int64, limit int) ([]*Message, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+messageCols+` FROM messages m LEFT JOIN message_bodies b ON b.message_pk = m.id
		WHERE m.thread_id = ? ORDER BY COALESCE(m.sent_at, m.received_at) DESC LIMIT ?`, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	// reverse to oldest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// SetState updates the processing state.
func (s *Store) SetState(ctx context.Context, id int64, state, lastErr string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE messages SET state = ?, last_error = ?, updated_at = ? WHERE id = ?",
		state, lastErr, ts(time.Now()), id)
	return err
}

// SetDelivery updates how the message reaches the user.
func (s *Store) SetDelivery(ctx context.Context, id int64, delivery string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE messages SET delivery = ?, updated_at = ? WHERE id = ?", delivery, ts(time.Now()), id)
	return err
}

// IncAttempts bumps the analysis attempt counter and returns the new value.
func (s *Store) IncAttempts(ctx context.Context, id int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "UPDATE messages SET attempts = attempts + 1 WHERE id = ? RETURNING attempts", id).Scan(&n)
	return n, err
}

// PendingIDs lists messages whose processing is unfinished, oldest first.
func (s *Store) PendingIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id FROM messages WHERE state = 'new' OR delivery = 'pending' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Analysis is a stored LLM analysis attempt.
type Analysis struct {
	ID             int64
	MessagePK      int64
	Attempt        int
	Provider       string
	Model          string
	PromptHash     string
	Status         string // ok | invalid | error
	Category       string
	Priority       string
	Summary        string
	ResultJSON     string
	AIInstructions bool
	Error          string
	CreatedAt      time.Time
}

// SaveAnalysis records one attempt.
func (s *Store) SaveAnalysis(ctx context.Context, a *Analysis) error {
	if a.ResultJSON == "" {
		a.ResultJSON = "{}"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO analyses(message_pk, attempt, provider, model, prompt_hash, status,
		category, priority, summary, result_json, ai_instructions, error) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.MessagePK, a.Attempt, a.Provider, a.Model, a.PromptHash, a.Status, a.Category, a.Priority, a.Summary,
		a.ResultJSON, b2i(a.AIInstructions), a.Error)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// LatestAnalysis returns the most recent successful analysis.
func (s *Store) LatestAnalysis(ctx context.Context, messagePK int64) (*Analysis, error) {
	var a Analysis
	var ai int
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, message_pk, attempt, provider, model, prompt_hash, status, category,
		priority, summary, result_json, ai_instructions, error, created_at FROM analyses
		WHERE message_pk = ? AND status = 'ok' ORDER BY id DESC LIMIT 1`, messagePK).
		Scan(&a.ID, &a.MessagePK, &a.Attempt, &a.Provider, &a.Model, &a.PromptHash, &a.Status, &a.Category,
			&a.Priority, &a.Summary, &a.ResultJSON, &ai, &a.Error, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.AIInstructions = ai == 1
	a.CreatedAt = parseTS(created)
	return &a, nil
}

// LLMCall is one provider invocation, for cost tracking.
type LLMCall struct {
	Task, Provider, Model string
	MessagePK             int64
	TokensIn, TokensOut   int64
	TokensCached          int64
	CostUSD               float64
	Latency               time.Duration
	Outcome               string
}

// RecordLLMCall stores token usage and estimated cost.
func (s *Store) RecordLLMCall(ctx context.Context, c LLMCall) error {
	var pk any
	if c.MessagePK > 0 {
		pk = c.MessagePK
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO llm_calls(task, provider, model, message_pk, tokens_in, tokens_out,
		tokens_cached, cost_usd_micros, latency_ms, outcome) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Task, c.Provider, c.Model, pk, c.TokensIn, c.TokensOut, c.TokensCached, int64(c.CostUSD*1e6),
		c.Latency.Milliseconds(), c.Outcome)
	return err
}

// LLMCostSince sums the estimated cost (USD) since t.
func (s *Store) LLMCostSince(ctx context.Context, t time.Time) (float64, int, error) {
	var micros, n int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(cost_usd_micros), 0), COUNT(*) FROM llm_calls WHERE created_at >= ?", ts(t)).Scan(&micros, &n)
	return float64(micros) / 1e6, int(n), err
}

// ClaimNotification reserves the (message, kind) pair before sending, so a
// crash between send and record cannot double-notify. Returns false if
// already claimed.
func (s *Store) ClaimNotification(ctx context.Context, messagePK int64, kind string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO notifications(message_pk, kind) VALUES(?, ?)", messagePK, kind)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetNotificationMessage stores the Telegram message id of a notification.
func (s *Store) SetNotificationMessage(ctx context.Context, messagePK int64, kind string, tgID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE notifications SET tg_message_id = ? WHERE message_pk = ? AND kind = ?", tgID, messagePK, kind)
	return err
}

// ReleaseNotification undoes a claim after a failed send.
func (s *Store) ReleaseNotification(ctx context.Context, messagePK int64, kind string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM notifications WHERE message_pk = ? AND kind = ? AND tg_message_id = 0", messagePK, kind)
	return err
}

// NotificationMessageID returns the Telegram message id of a notification.
func (s *Store) NotificationMessageID(ctx context.Context, messagePK int64, kind string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT tg_message_id FROM notifications WHERE message_pk = ? AND kind = ?", messagePK, kind).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ClaimDigest reserves a digest slot; false if it was already sent.
func (s *Store) ClaimDigest(ctx context.Context, slot string) (int64, bool, error) {
	res, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO digests(slot) VALUES(?)", slot)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	id, _ := res.LastInsertId()
	return id, true, nil
}

// MarkDigestSent records a digest as sent.
func (s *Store) MarkDigestSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE digests SET sent_at = ? WHERE id = ?", ts(time.Now()), id)
	return err
}

// DigestItem is a message waiting for the next digest.
type DigestItem struct {
	Message  *Message
	Analysis *Analysis // nil when not analysed (rule skip, failure)
}

// DigestBacklog returns messages routed to the digest that no digest has
// included yet.
func (s *Store) DigestBacklog(ctx context.Context, limit int) ([]DigestItem, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+messageCols+` FROM messages m LEFT JOIN message_bodies b ON b.message_pk = m.id
		WHERE m.delivery = 'digest' AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.message_pk = m.id AND n.kind = 'digest')
		ORDER BY m.received_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var msgs []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DigestItem, 0, len(msgs))
	for _, m := range msgs {
		a, err := s.LatestAnalysis(ctx, m.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		out = append(out, DigestItem{Message: m, Analysis: a})
	}
	return out, nil
}

// MarkDigested records that the given messages went out in digest id.
func (s *Store) MarkDigested(ctx context.Context, digestID int64, messagePKs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, pk := range messagePKs {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO notifications(message_pk, kind, digest_id) VALUES(?, 'digest', ?)", pk, digestID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Callback is an inline-button action bound to an opaque token.
type Callback struct {
	Token     string
	Action    string
	MessagePK int64
	DraftPK   int64
	Arg       string
	ExpiresAt time.Time
	UsedAt    time.Time
}

// CreateCallback stores an action and returns its random token. The token
// expires at expiresAt (computed with the caller's clock).
func (s *Store) CreateCallback(ctx context.Context, action string, messagePK, draftPK int64, arg string, expiresAt time.Time) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	var mpk, dpk any
	if messagePK > 0 {
		mpk = messagePK
	}
	if draftPK > 0 {
		dpk = draftPK
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO callbacks(token, action, message_pk, draft_pk, arg, expires_at) VALUES(?, ?, ?, ?, ?, ?)",
		token, action, mpk, dpk, arg, ts(expiresAt))
	return token, err
}

// Callback errors.
var (
	ErrCallbackUsed    = errors.New("store: callback already used")
	ErrCallbackExpired = errors.New("store: callback expired")
)

// ConsumeCallback atomically marks a token used and returns its action. A
// token works once; an expired or reused token is refused.
func (s *Store) ConsumeCallback(ctx context.Context, token string, now time.Time) (*Callback, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var c Callback
	var mpk, dpk sql.NullInt64
	var exp string
	var used sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT token, action, message_pk, draft_pk, arg, expires_at, used_at FROM callbacks WHERE token = ?", token).
		Scan(&c.Token, &c.Action, &mpk, &dpk, &c.Arg, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.MessagePK, c.DraftPK, c.ExpiresAt = mpk.Int64, dpk.Int64, parseTS(exp)
	if used.Valid {
		return &c, ErrCallbackUsed
	}
	if now.After(c.ExpiresAt) {
		return &c, ErrCallbackExpired
	}
	if _, err := tx.ExecContext(ctx, "UPDATE callbacks SET used_at = ? WHERE token = ? AND used_at IS NULL", ts(now), token); err != nil {
		return nil, err
	}
	return &c, tx.Commit()
}

// Stats summarises activity for /status.
type Stats struct {
	MessagesToday   int
	ImmediateToday  int
	DigestBacklog   int
	AnalysisFailed  int
	LLMCallsToday   int
	LLMCostTodayUSD float64
}

// GetStats computes counters since the given local midnight.
func (s *Store) GetStats(ctx context.Context, since time.Time) (Stats, error) {
	var st Stats
	q := func(query string, dst *int, args ...any) error {
		return s.db.QueryRowContext(ctx, query, args...).Scan(dst)
	}
	if err := q("SELECT COUNT(*) FROM messages WHERE received_at >= ? AND delivery != 'silent'", &st.MessagesToday, ts(since)); err != nil {
		return st, err
	}
	if err := q("SELECT COUNT(*) FROM notifications WHERE kind = 'immediate' AND sent_at >= ?", &st.ImmediateToday, ts(since)); err != nil {
		return st, err
	}
	if err := q(`SELECT COUNT(*) FROM messages m WHERE m.delivery = 'digest'
		AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.message_pk = m.id AND n.kind = 'digest')`, &st.DigestBacklog); err != nil {
		return st, err
	}
	if err := q("SELECT COUNT(*) FROM messages WHERE state = 'analysis_failed' AND received_at >= ?", &st.AnalysisFailed, ts(since)); err != nil {
		return st, err
	}
	var err error
	st.LLMCostTodayUSD, st.LLMCallsToday, err = s.LLMCostSince(ctx, since)
	return st, err
}

// PurgeBodies empties bodies of messages received before t. Summaries,
// analyses and actions are kept.
func (s *Store) PurgeBodies(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE message_bodies SET text_clean = '', text_full = '', purged_at = ?
		WHERE purged_at IS NULL AND message_pk IN (SELECT id FROM messages WHERE received_at < ?)`, ts(time.Now()), ts(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeCallbacks deletes callback tokens that expired before t.
func (s *Store) PurgeCallbacks(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM callbacks WHERE expires_at < ?", ts(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MessageByNotification finds the message behind a Telegram notification.
func (s *Store) MessageByNotification(ctx context.Context, tgMessageID int64) (*Message, error) {
	var pk int64
	err := s.db.QueryRowContext(ctx, "SELECT message_pk FROM notifications WHERE tg_message_id = ? AND tg_message_id != 0 LIMIT 1", tgMessageID).Scan(&pk)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetMessage(ctx, pk)
}
