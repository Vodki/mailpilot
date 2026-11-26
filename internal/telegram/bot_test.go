package telegram

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/telegram/tgtest"
)

type recHandler struct {
	mu    sync.Mutex
	cmds  []string
	texts []string
	cbs   []string
}

func (h *recHandler) Command(_ context.Context, name, args string, _ *Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cmds = append(h.cmds, name+"|"+args)
}

func (h *recHandler) Text(_ context.Context, m *Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.texts = append(h.texts, m.Text)
}

func (h *recHandler) Callback(_ context.Context, q *CallbackQuery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cbs = append(h.cbs, q.Data)
}

func (h *recHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.cmds) + len(h.texts) + len(h.cbs)
}

func TestBotOnlyAnswersConfiguredChat(t *testing.T) {
	srv := tgtest.New(t)
	h := &recHandler{}
	var rejected []int64
	var mu sync.Mutex
	var saved int64
	b := &Bot{
		Client: New(srv.URL, config.NewSecret("T"), nil), ChatID: 1000, Handler: h,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), PollTimeout: time.Second,
		Rejected: func(_ context.Context, chat, _ int64, _ string) {
			mu.Lock()
			rejected = append(rejected, chat)
			mu.Unlock()
		},
		SaveOffset: func(_ context.Context, o int64) { mu.Lock(); saved = o; mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	srv.Push(tgtest.PrivateMessage(666, 666, "/status"))           // stranger
	srv.Push(tgtest.CallbackPress(666, 666, "tok"))                // stranger's button
	srv.Push(tgtest.CallbackPress(1000, 666, "tok"))               // right chat, wrong user
	srv.Push(tgtest.PrivateMessage(1000, 1000, "/Status@bot now")) // Louis
	srv.Push(tgtest.PrivateMessage(1000, 1000, "bonjour"))
	srv.Push(tgtest.CallbackPress(1000, 1000, "abc"))

	tgtest.WaitFor(t, 5*time.Second, func() bool { return h.count() == 3 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmds[0] != "status|now" || h.texts[0] != "bonjour" || h.cbs[0] != "abc" {
		t.Fatalf("handled %v %v %v", h.cmds, h.texts, h.cbs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rejected) != 3 || saved != 7 {
		t.Fatalf("rejected %v saved offset %d", rejected, saved)
	}
	// Rejected callbacks are acknowledged silently, nothing else is sent.
	if n := len(srv.Calls("sendMessage")); n != 0 {
		t.Fatalf("bot replied %d times to strangers", n)
	}
}

func TestAuthorizedRefusesGroups(t *testing.T) {
	b := &Bot{ChatID: 1000}
	if b.Authorized(Chat{ID: 1000, Type: "group"}, &User{ID: 1000}) {
		t.Fatal("group chats must be refused")
	}
	if !b.Authorized(Chat{ID: 1000, Type: "private"}, &User{ID: 1000}) {
		t.Fatal("private chat refused")
	}
}

func TestSendMessageHasNoParseModeAndNoPreview(t *testing.T) {
	srv := tgtest.New(t)
	c := New(srv.URL, config.NewSecret("T"), nil)
	var b Builder
	b.Bold("Titre").Plain(" <b>pas du HTML</b> https://evil.example")
	if _, err := c.SendMessage(context.Background(), 1000, b.Text(), SendOptions{}); err != nil {
		t.Fatal(err)
	}
	p := srv.Calls("sendMessage")[0].Params
	if _, ok := p["parse_mode"]; ok {
		t.Fatal("parse_mode must never be set")
	}
	lp, _ := p["link_preview_options"].(map[string]any)
	if lp["is_disabled"] != true {
		t.Fatalf("link previews must be disabled: %v", p)
	}
	if p["text"] != "Titre <b>pas du HTML</b> https://evil.example" {
		t.Fatalf("text %q", p["text"])
	}
}

func TestBuilderUTF16Offsets(t *testing.T) {
	var b Builder
	b.Plain("🔴 é ").Bold("Gras").Plain("\u202eabc\x00")
	txt := b.Text()
	// 🔴 is 2 UTF-16 units, so "🔴 é " is 5 units.
	if len(txt.Entities) != 1 || txt.Entities[0].Offset != 5 || txt.Entities[0].Length != 4 {
		t.Fatalf("entities %+v", txt.Entities)
	}
	if txt.Body != "🔴 é Grasabc" {
		t.Fatalf("control and bidi characters must be stripped: %q", txt.Body)
	}
}

func TestBuilderTruncatesAndSplit(t *testing.T) {
	var b Builder
	for range 1000 {
		b.Bold("abcdefgh")
	}
	txt := b.Text()
	if u16len(txt.Body) > MaxText {
		t.Fatalf("length %d", u16len(txt.Body))
	}
	last := txt.Entities[len(txt.Entities)-1]
	if last.Offset+last.Length > u16len(txt.Body) {
		t.Fatal("entity beyond text")
	}
	var lines []Text
	for range 50 {
		var l Builder
		l.Bold("x").Plain(" ligne assez longue pour remplir le message rapidement, encore et encore")
		lines = append(lines, l.Text())
	}
	chunks := Split(lines, 1000)
	if len(chunks) < 3 {
		t.Fatalf("chunks %d", len(chunks))
	}
	for _, c := range chunks {
		if u16len(c.Body) > 1000 {
			t.Fatal("chunk too long")
		}
		for _, e := range c.Entities {
			if c.Body[e.Offset:e.Offset+1] != "x" {
				t.Fatalf("entity misplaced at %d", e.Offset)
			}
		}
	}
}
