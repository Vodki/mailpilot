// Package imapwatch keeps one long IMAP connection on the monitored mailbox.
// The mailbox is opened with EXAMINE (read-only) and bodies are fetched with
// BODY.PEEK[], so the watcher can never change a message's \Seen flag or any
// other flag.
package imapwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/mailbox"
	"github.com/Vodki/mailpilot/internal/parse"
)

// Fetched is one message as delivered to the sink.
type Fetched struct {
	Mailbox      string
	UIDValidity  uint32
	UID          uint32
	InternalDate time.Time
	Size         int64
	Raw          []byte // nil when the message exceeds parse.MaxRawSize (headers only)
	HeadersOnly  bool
	// Silent is set for the bootstrap import and for messages older than the
	// last successful sync after a UIDVALIDITY change: stored, never notified.
	Silent bool
}

// Cursor is the persisted synchronisation state.
type Cursor struct {
	UIDValidity   uint32
	LastUID       uint32
	LastOK        time.Time
	BootstrapDone bool
}

// Sink receives messages and persists the cursor. Implementations must be
// idempotent: a message may be delivered twice after a crash.
type Sink interface {
	LoadCursor(ctx context.Context, mailbox string) (Cursor, error)
	SaveCursor(ctx context.Context, mailbox string, c Cursor) error
	Deliver(ctx context.Context, f *Fetched) error
}

// Health is a snapshot of the watcher state for /status and the watchdog.
type Health struct {
	Connected  bool
	Mode       string // idle | poll
	LastOK     time.Time
	LastError  string
	AuthFailed bool
	Since      time.Time // start of the current failure streak
}

// Watcher runs the sync loop.
type Watcher struct {
	cfg  config.IMAPConfig
	sink Sink
	log  *slog.Logger
	now  func() time.Time

	forcePoll bool // tests: ignore IDLE

	mu     sync.Mutex
	health Health
	kick   chan struct{}
}

// New builds a watcher.
func New(cfg config.IMAPConfig, sink Sink, log *slog.Logger) *Watcher {
	return &Watcher{cfg: cfg, sink: sink, log: log.With("component", "imapwatch"), now: time.Now, kick: make(chan struct{}, 1)}
}

// Health returns the current state.
func (w *Watcher) Health() Health {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.health
}

// Kick asks for an immediate resync (e.g. /status refresh). Non-blocking.
func (w *Watcher) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *Watcher) setHealth(f func(h *Health)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(&w.health)
}

// ErrAuth wraps a login failure.
var ErrAuth = errors.New("imap authentication failed")

// Run loops until ctx is cancelled, reconnecting with exponential backoff
// and jitter.
func (w *Watcher) Run(ctx context.Context) error {
	backoff := 5 * time.Second
	for {
		before := w.Health().LastOK
		err := w.session(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutting down: the session error is expected
		}
		w.setHealth(func(h *Health) {
			h.Connected = false
			h.LastError = errString(err)
			h.AuthFailed = errors.Is(err, ErrAuth)
			if h.Since.IsZero() {
				h.Since = w.now()
			}
		})
		w.log.Warn("imap session ended", "err", errString(err), "retry_in", backoff.String())
		// Jitter in [0.5, 1.5) x backoff.
		d := time.Duration(float64(backoff) * (0.5 + rand.Float64())) //nolint:gosec // jitter, not crypto
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
		backoff *= 2
		if errors.Is(err, ErrAuth) {
			// Do not hammer the server with bad credentials (account lockout).
			backoff = max(backoff, 5*time.Minute)
		}
		if backoff > 10*time.Minute {
			backoff = 10 * time.Minute
		}
		if w.Health().LastOK.After(before) && !errors.Is(err, ErrAuth) {
			backoff = 5 * time.Second // the session worked for a while
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "connection closed"
	}
	return err.Error()
}

// session runs one connection: login, EXAMINE, sync, then IDLE/poll loop.
func (w *Watcher) session(ctx context.Context) error {
	changed := make(chan struct{}, 1)
	notify := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					notify()
				}
			},
			Expunge: func(uint32) {},
		},
	}
	c, err := mailbox.Dial(ctx, w.cfg, opts)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	// Close the connection when ctx ends so blocked reads return.
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	if err := c.Login(w.cfg.Username, w.cfg.Password.Reveal()).Wait(); err != nil {
		return fmt.Errorf("%w: %v", ErrAuth, err) //nolint:errorlint // keep sentinel, message only
	}
	caps, err := c.Capability().Wait()
	if err != nil {
		return fmt.Errorf("capability: %w", err)
	}
	useIdle := (caps.Has(imap.CapIdle) || caps.Has(imap.CapIMAP4rev2)) && !w.forcePoll
	sel, err := c.Select(w.cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return fmt.Errorf("examine %s: %w", w.cfg.Mailbox, err)
	}
	mode := "poll"
	if useIdle {
		mode = "idle"
	}
	w.log.Info("imap connected", "mailbox", w.cfg.Mailbox, "mode", mode, "uidvalidity", sel.UIDValidity, "exists", sel.NumMessages)
	w.setHealth(func(h *Health) { h.Connected, h.Mode = true, mode })

	for {
		if err := w.sync(ctx, c, sel.UIDValidity, uint32(sel.UIDNext)); err != nil {
			return err
		}
		now := w.now()
		w.setHealth(func(h *Health) { h.LastOK, h.LastError, h.AuthFailed, h.Since = now, "", false, time.Time{} })

		if useIdle {
			if err := w.idle(ctx, c, changed); err != nil {
				return err
			}
		} else {
			select {
			case <-ctx.Done():
				return nil
			case <-w.kick:
			case <-time.After(w.cfg.PollInterval):
			}
			// NOOP lets the server report EXISTS for the selected mailbox.
			if err := c.Noop().Wait(); err != nil {
				return fmt.Errorf("noop: %w", err)
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// idle waits for a mailbox change, a kick, or the refresh interval.
func (w *Watcher) idle(ctx context.Context, c *imapclient.Client, changed <-chan struct{}) error {
	cmd, err := c.Idle()
	if err != nil {
		return fmt.Errorf("idle: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
	case <-changed:
	case <-w.kick:
	case <-time.After(w.cfg.IdleRefresh):
	case err := <-done:
		return fmt.Errorf("idle ended: %w", err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("idle close: %w", err)
	}
	if err := <-done; err != nil {
		return fmt.Errorf("idle: %w", err)
	}
	return nil
}

const fetchBatch = 25

// sync fetches everything after the persisted cursor.
func (w *Watcher) sync(ctx context.Context, c *imapclient.Client, uidValidity, uidNext uint32) error {
	mbox := w.cfg.Mailbox
	cur, err := w.sink.LoadCursor(ctx, mbox)
	if err != nil {
		return fmt.Errorf("load cursor: %w", err)
	}

	var uids []imap.UID
	silentBefore := time.Time{} // messages with INTERNALDATE before this are silent
	allSilent := false
	switch {
	case !cur.BootstrapDone:
		// First run: import the last N days without analysis or notification.
		if w.cfg.BootstrapDays > 0 {
			uids, err = searchSince(c, w.now().AddDate(0, 0, -w.cfg.BootstrapDays))
		}
		allSilent = true
		w.log.Info("bootstrap import", "messages", len(uids), "days", w.cfg.BootstrapDays)
	case cur.UIDValidity != uidValidity:
		// UIDs were renumbered: rescan a window and rely on Message-ID dedupe.
		// Messages older than the last successful sync were already seen.
		since := cur.LastOK.AddDate(0, 0, -1)
		if floor := w.now().AddDate(0, 0, -w.cfg.BootstrapDays); since.Before(floor) {
			since = floor
		}
		uids, err = searchSince(c, since)
		silentBefore = cur.LastOK
		w.log.Warn("uidvalidity changed", "old", cur.UIDValidity, "new", uidValidity, "rescan", len(uids))
	default:
		uids, err = searchAfter(c, cur.LastUID)
	}
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	newCur := Cursor{UIDValidity: uidValidity, LastUID: cur.LastUID, BootstrapDone: true}
	if !cur.BootstrapDone || cur.UIDValidity != uidValidity {
		// Everything below UIDNEXT was either imported from the window or
		// deliberately left out (older than the window): never fetch it later.
		newCur.LastUID = 0
		if uidNext > 0 {
			newCur.LastUID = uidNext - 1
		}
	}
	for start := 0; start < len(uids); start += fetchBatch {
		end := min(start+fetchBatch, len(uids))
		msgs, err := fetch(c, uids[start:end])
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		for _, f := range msgs {
			f.Mailbox, f.UIDValidity = mbox, uidValidity
			f.Silent = allSilent || (!silentBefore.IsZero() && f.InternalDate.Before(silentBefore))
			if err := w.sink.Deliver(ctx, f); err != nil {
				return fmt.Errorf("deliver uid %d: %w", f.UID, err)
			}
			if f.UID > newCur.LastUID {
				newCur.LastUID = f.UID
			}
		}
		// Persist progress after each batch so a crash resumes here.
		newCur.LastOK = w.now()
		if err := w.sink.SaveCursor(ctx, mbox, newCur); err != nil {
			return fmt.Errorf("save cursor: %w", err)
		}
	}
	newCur.LastOK = w.now()
	return w.sink.SaveCursor(ctx, mbox, newCur)
}

func searchSince(c *imapclient.Client, since time.Time) ([]imap.UID, error) {
	data, err := c.UIDSearch(&imap.SearchCriteria{Since: since}, nil).Wait()
	if err != nil {
		return nil, err
	}
	return data.AllUIDs(), nil
}

func searchAfter(c *imapclient.Client, last uint32) ([]imap.UID, error) {
	var set imap.UIDSet
	set.AddRange(imap.UID(last+1), 0) // last+1:*
	data, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, err
	}
	// "n:*" always matches the highest UID, even if it is <= last.
	var out []imap.UID
	for _, u := range data.AllUIDs() {
		if uint32(u) > last {
			out = append(out, u)
		}
	}
	return out, nil
}

// fetch reads size first, then the full body with BODY.PEEK[] (or only the
// header for oversized messages). PEEK never sets \Seen.
func fetch(c *imapclient.Client, uids []imap.UID) ([]*Fetched, error) {
	set := imap.UIDSetNum(uids...)
	meta, err := c.Fetch(set, &imap.FetchOptions{UID: true, InternalDate: true, RFC822Size: true}).Collect()
	if err != nil {
		return nil, err
	}
	var small, big []imap.UID
	info := map[imap.UID]*Fetched{}
	for _, m := range meta {
		f := &Fetched{UID: uint32(m.UID), InternalDate: m.InternalDate, Size: m.RFC822Size}
		info[m.UID] = f
		if m.RFC822Size > parse.MaxRawSize {
			big = append(big, m.UID)
		} else {
			small = append(small, m.UID)
		}
	}
	full := &imap.FetchItemBodySection{Peek: true}
	header := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	for _, group := range []struct {
		uids    []imap.UID
		section *imap.FetchItemBodySection
	}{{small, full}, {big, header}} {
		if len(group.uids) == 0 {
			continue
		}
		bufs, err := c.Fetch(imap.UIDSetNum(group.uids...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{group.section}}).Collect()
		if err != nil {
			return nil, err
		}
		for _, b := range bufs {
			f := info[b.UID]
			if f == nil {
				continue
			}
			f.Raw = b.FindBodySection(group.section)
			f.HeadersOnly = group.section == header
		}
	}
	out := make([]*Fetched, 0, len(uids))
	for _, u := range uids {
		if f := info[u]; f != nil && f.Raw != nil {
			out = append(out, f)
		}
	}
	return out, nil
}
