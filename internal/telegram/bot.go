package telegram

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Handler receives updates that passed the chat filter.
type Handler interface {
	Command(ctx context.Context, name, args string, m *Message)
	Text(ctx context.Context, m *Message)
	Callback(ctx context.Context, q *CallbackQuery)
}

// Bot runs the long-polling loop. It only ever reacts to the configured
// chat; everything else is dropped and reported through Rejected.
type Bot struct {
	Client  *Client
	ChatID  int64
	Handler Handler
	Log     *slog.Logger
	// Offset persistence, so a restart does not replay commands.
	LoadOffset func(ctx context.Context) int64
	SaveOffset func(ctx context.Context, offset int64)
	// Rejected is called for updates from any other chat or user (audit).
	Rejected func(ctx context.Context, chatID, userID int64, kind string)
	// PollTimeout is the getUpdates long-poll timeout (default 50 s).
	PollTimeout time.Duration
}

// Authorized reports whether an update comes from Louis's chat. In a
// private chat the chat id equals the user id; in a group we additionally
// require the sender to be that same id, so other members cannot act.
func (b *Bot) Authorized(chat Chat, from *User) bool {
	if chat.ID != b.ChatID {
		return false
	}
	if chat.Type != "private" && chat.Type != "" {
		return false // groups and channels are refused outright
	}
	return from == nil || from.ID == b.ChatID
}

// Run polls until ctx ends.
func (b *Bot) Run(ctx context.Context) {
	timeout := b.PollTimeout
	if timeout <= 0 {
		timeout = 50 * time.Second
	}
	var offset int64
	if b.LoadOffset != nil {
		offset = b.LoadOffset(ctx)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := b.Client.GetUpdates(ctx, offset, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.Log.Warn("telegram getUpdates", "err", err, "retry_in", backoff.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		for i := range ups {
			u := &ups[i]
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.dispatch(ctx, u)
		}
		if len(ups) > 0 && b.SaveOffset != nil {
			b.SaveOffset(ctx, offset)
		}
	}
}

func (b *Bot) dispatch(ctx context.Context, u *Update) {
	defer func() {
		if r := recover(); r != nil {
			b.Log.Error("telegram handler panic", "update_id", u.UpdateID, "panic", r)
		}
	}()
	switch {
	case u.Message != nil:
		m := u.Message
		if !b.Authorized(m.Chat, m.From) {
			b.reject(ctx, m.Chat.ID, m.From, "message")
			return
		}
		text := strings.TrimSpace(m.Text)
		if strings.HasPrefix(text, "/") {
			name, args, _ := strings.Cut(text[1:], " ")
			name, _, _ = strings.Cut(name, "@") // /cmd@botname
			b.Handler.Command(ctx, strings.ToLower(name), strings.TrimSpace(args), m)
			return
		}
		b.Handler.Text(ctx, m)
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if q.Message == nil || !b.Authorized(q.Message.Chat, &q.From) {
			var chat int64
			if q.Message != nil {
				chat = q.Message.Chat.ID
			}
			b.reject(ctx, chat, &q.From, "callback")
			_ = b.Client.AnswerCallback(ctx, q.ID, "")
			return
		}
		b.Handler.Callback(ctx, q)
	}
}

func (b *Bot) reject(ctx context.Context, chatID int64, from *User, kind string) {
	var uid int64
	if from != nil {
		uid = from.ID
	}
	b.Log.Warn("telegram update from unauthorized chat ignored", "chat_id", chatID, "user_id", uid, "kind", kind)
	if b.Rejected != nil {
		b.Rejected(ctx, chatID, uid, kind)
	}
}
