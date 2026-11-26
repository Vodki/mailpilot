// Package app wires the pipeline: IMAP watcher -> parse -> store -> analysis
// -> policy -> Telegram. Only this package (and the packages it calls on
// Louis's explicit action) decides and executes actions; the LLM output is
// data that Go code validates and interprets.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/imapwatch"
	"github.com/Vodki/mailpilot/internal/llm"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

// Settings keys.
const (
	settingPaused   = "paused"
	settingTGOffset = "telegram_offset"
)

// Notifier is the outbound Telegram surface used by the app (an interface
// so tests can capture messages).
type Notifier interface {
	SendMessage(ctx context.Context, chatID int64, t telegram.Text, o telegram.SendOptions) (*telegram.Message, error)
	EditMessageText(ctx context.Context, chatID, messageID int64, t telegram.Text, markup *telegram.InlineMarkup) error
	EditMarkup(ctx context.Context, chatID, messageID int64, markup *telegram.InlineMarkup) error
	AnswerCallback(ctx context.Context, id, text string) error
}

// Watcher is the IMAP health surface.
type Watcher interface {
	Health() imapwatch.Health
	Kick()
}

// Deps are the external collaborators.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Provider llm.Provider
	Prompts  analysis.Prompts
	TG       Notifier
	Log      *slog.Logger
	Version  string
	Now      func() time.Time
}

// App is the running service.
type App struct {
	cfg      *config.Config
	st       *store.Store
	provider llm.Provider
	analyzer *analysis.Analyzer
	tg       Notifier
	log      *slog.Logger
	version  string
	now      func() time.Time
	started  time.Time
	self     map[string]bool // Louis's own addresses, lower case

	watcher Watcher
	paused  atomic.Bool
	queue   chan int64
	wg      sync.WaitGroup

	healthMu      sync.Mutex
	healthAlerted bool
}

// New builds the app. Call SetWatcher before Run.
func New(d Deps) (*App, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	an, err := analysis.New(d.Provider, d.Prompts, d.Config.Location)
	if err != nil {
		return nil, err
	}
	a := &App{
		cfg: d.Config, st: d.Store, provider: d.Provider, analyzer: an, tg: d.TG,
		log: d.Log, version: d.Version, now: d.Now, started: d.Now(),
		self:  map[string]bool{strings.ToLower(d.Config.IMAP.Username): true},
		queue: make(chan int64, 1024),
	}
	for _, s := range d.Config.SelfAliases {
		a.self[strings.ToLower(strings.TrimSpace(s))] = true
	}
	return a, nil
}

// SetWatcher attaches the IMAP watcher (for /status and the watchdog).
func (a *App) SetWatcher(w Watcher) { a.watcher = w }

// Paused reports whether outbound actions are suspended.
func (a *App) Paused() bool { return a.paused.Load() }

// Start loads persisted state and launches the background workers. It
// returns immediately; Wait blocks until ctx is done and workers exit.
func (a *App) Start(ctx context.Context) error {
	v, err := a.st.GetSetting(ctx, settingPaused)
	if err != nil {
		return err
	}
	a.paused.Store(v == "1")
	if err := a.startExtensions(ctx); err != nil {
		return err
	}
	a.goRun(func() { a.worker(ctx) })
	a.goRun(func() { a.scheduler(ctx) })
	// Resume unfinished work from before a restart.
	a.sweep(ctx)
	return nil
}

func (a *App) goRun(f func()) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		f()
	}()
}

// Wait blocks until all workers have exited.
func (a *App) Wait() { a.wg.Wait() }

// Enqueue schedules a message for processing. Non-blocking: if the queue is
// full, the periodic sweep picks the message up later.
func (a *App) Enqueue(id int64) {
	select {
	case a.queue <- id:
	default:
		a.log.Warn("queue full, deferring", "message_pk", id)
	}
}

func (a *App) sweep(ctx context.Context) {
	ids, err := a.st.PendingIDs(ctx)
	if err != nil {
		a.log.Error("pending sweep", "err", err)
		return
	}
	for _, id := range ids {
		a.Enqueue(id)
	}
}

func (a *App) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-a.queue:
			a.process(ctx, id)
		}
	}
}

// send posts to Louis's chat.
func (a *App) send(ctx context.Context, t telegram.Text, o telegram.SendOptions) (*telegram.Message, error) {
	return a.tg.SendMessage(ctx, a.cfg.Telegram.ChatID, t, o)
}

// sendText posts a short plain message and logs failures.
func (a *App) sendText(ctx context.Context, s string) {
	var b telegram.Builder
	b.Plain(s)
	if _, err := a.send(ctx, b.Text(), telegram.SendOptions{}); err != nil {
		a.log.Warn("telegram send", "err", err)
	}
}

func (a *App) audit(ctx context.Context, actor, event, ref string) {
	if err := a.st.Audit(ctx, actor, event, ref); err != nil {
		a.log.Error("audit", "event", event, "err", err)
	}
}

// localDay returns local midnight of t.
func (a *App) localDay(t time.Time) time.Time {
	t = t.In(a.cfg.Location)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, a.cfg.Location)
}

// Bot returns the Telegram long-polling loop bound to this app.
func (a *App) Bot(c *telegram.Client) *telegram.Bot {
	return &telegram.Bot{
		Client: c, ChatID: a.cfg.Telegram.ChatID, Handler: a, Log: a.log.With("component", "telegram"),
		LoadOffset: func(ctx context.Context) int64 {
			v, _ := a.st.GetSetting(ctx, settingTGOffset)
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		},
		SaveOffset: func(ctx context.Context, off int64) {
			if err := a.st.SetSetting(ctx, settingTGOffset, strconv.FormatInt(off, 10)); err != nil {
				a.log.Error("save telegram offset", "err", err)
			}
		},
		Rejected: func(ctx context.Context, chatID, userID int64, kind string) {
			a.audit(ctx, "telegram", "rejected_"+kind, fmt.Sprintf("chat=%d user=%d", chatID, userID))
		},
	}
}

// Announce tells Louis the service (re)started and in which mode.
func (a *App) Announce(ctx context.Context) {
	pause, send := "non", "désactivé"
	if a.Paused() {
		pause = "OUI (/resume pour reprendre)"
	}
	if a.cfg.SMTP.SendEnabled {
		send = "activé, avec double validation"
	}
	a.sendText(ctx, "▶️ mailpilot "+a.version+" démarré.\nPause : "+pause+"\nEnvoi SMTP : "+send)
}
