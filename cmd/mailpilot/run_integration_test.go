//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/telegram/tgtest"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// TestRunEndToEnd runs the full service against GreenMail, a fake Telegram
// API and a fake claude: a new urgent mail must reach Telegram quickly, the
// mailbox flags must stay untouched and /status must answer.
func TestRunEndToEnd(t *testing.T) {
	path := testConfig(t)
	tg := tgtest.New(t)
	fake, _ := filepath.Abs("testdata/fake-claude-triage.sh")
	extra := "\ntelegram:\n  chat_id: 4242\n  api_base: " + tg.URL +
		"\nllm:\n  claude_cli:\n    binary: " + fake + "\n    timeout: 10s\n"
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(extra)
	_ = f.Close()
	t.Setenv(config.EnvTelegramToken, "TEST")
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, cfg) // pre-existing mail: must be imported silently
	time.Sleep(500 * time.Millisecond)
	before := unseenCount(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- cmdRun(ctx, []string{"-config", path}, logs) }()
	defer func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("run exited %d", code)
			}
		case <-time.After(20 * time.Second):
			t.Error("run did not stop")
		}
		if strings.Contains(logs.String(), "Corps") || strings.Contains(logs.String(), "Bonjour") {
			t.Error("mail content leaked into logs")
		}
	}()

	has := func(sub string) func() bool {
		return func() bool {
			for _, s := range tg.Texts() {
				if strings.Contains(s, sub) {
					return true
				}
			}
			return false
		}
	}
	tgtest.WaitFor(t, 30*time.Second, has("démarré"))
	tgtest.WaitFor(t, 30*time.Second, func() bool { return strings.Contains(logs.String(), "bootstrap import") })

	subject := "URGENT-TEST " + time.Now().Format("150405")
	start := time.Now()
	deliverSubject(t, cfg, subject)
	tgtest.WaitFor(t, 60*time.Second, has(subject))
	t.Logf("notification after %s", time.Since(start))
	if has("Test phase 0")() {
		t.Fatal("bootstrap mail must not be notified")
	}

	tg.Push(tgtest.PrivateMessage(4242, 4242, "/status"))
	tgtest.WaitFor(t, 30*time.Second, has("IMAP : connecté (IDLE)"))
	tg.Push(tgtest.PrivateMessage(999, 999, "/pause"))
	time.Sleep(3 * time.Second)
	if has("⏸")() {
		t.Fatal("a stranger paused the bot")
	}

	if after := unseenCount(t, cfg); after != before+1 {
		t.Fatalf("\\Seen flags changed: unseen %d -> %d (one new mail)", before, after)
	}
}

func buttonToken(c tgtest.Call, prefix string) string {
	rm, _ := c.Params["reply_markup"].(map[string]any)
	rows, _ := rm["inline_keyboard"].([]any)
	for _, r := range rows {
		for _, b := range r.([]any) {
			bm := b.(map[string]any)
			if strings.HasPrefix(bm["text"].(string), prefix) {
				return bm["callback_data"].(string)
			}
		}
	}
	return ""
}

func findCall(tg *tgtest.Server, sub, button string) func() (string, bool) {
	return func() (string, bool) {
		calls := tg.Calls("sendMessage")
		for i := len(calls) - 1; i >= 0; i-- {
			if strings.Contains(calls[i].Params["text"].(string), sub) {
				if tok := buttonToken(calls[i], button); tok != "" {
					return tok, true
				}
			}
		}
		return "", false
	}
}

func waitToken(t *testing.T, f func() (string, bool)) string {
	t.Helper()
	var tok string
	tgtest.WaitFor(t, 60*time.Second, func() bool {
		var ok bool
		tok, ok = f()
		return ok
	})
	return tok
}

// TestDraftAndSendEndToEnd drives the whole reply flow through Telegram
// buttons against GreenMail: draft in Drafts, double confirmation, SMTP
// delivery with threading headers, copy in Sent, draft removed.
func TestDraftAndSendEndToEnd(t *testing.T) {
	path := testConfig(t)
	cfg0, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ensureFolders(t, cfg0, "Drafts", "Sent")
	tg := tgtest.New(t)
	fake, _ := filepath.Abs("testdata/fake-claude-triage.sh")
	raw, _ := os.ReadFile(path)
	conf := strings.Replace(string(raw), "imap:\n", "imap:\n  drafts_folder: Drafts\n  sent_folder: Sent\n", 1)
	conf = strings.Replace(conf, "smtp:\n", "smtp:\n  send_enabled: true\n", 1)
	conf += "\ntelegram:\n  chat_id: 4242\n  api_base: " + tg.URL + "\nllm:\n  claude_cli:\n    binary: " + fake + "\n    timeout: 10s\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvTelegramToken, "TEST")
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- cmdRun(ctx, []string{"-config", path}, logs) }()
	defer func() {
		cancel()
		<-done
		if t.Failed() {
			t.Logf("logs:\n%s", logs.String())
			for _, s := range tg.Texts() {
				t.Logf("telegram: %s", s)
			}
		}
	}()
	tgtest.WaitFor(t, 30*time.Second, func() bool { return strings.Contains(logs.String(), "bootstrap import") })

	subject := "URGENT-TEST draft " + time.Now().Format("150405.000")
	deliverSubject(t, cfg, subject)
	tg.Push(tgtest.CallbackPress(4242, 4242, waitToken(t, findCall(tg, subject, "✍️ Brouillon"))))
	tg.Push(tgtest.CallbackPress(4242, 4242, waitToken(t, findCall(tg, "✍️ Brouillon v1", "✉️ Envoyer"))))
	tg.Push(tgtest.CallbackPress(4242, 4242, waitToken(t, findCall(tg, "Confirmer l'envoi", "✅"))))
	tgtest.WaitFor(t, 60*time.Second, func() bool {
		for _, s := range tg.Texts() {
			if strings.Contains(s, "Réponse envoyée à pierre@example.test") {
				return true
			}
		}
		return false
	})

	pierre := cfg.IMAP
	pierre.Username = "pierre@example.test"
	if n := countSubject(t, pierre, "INBOX", "RE: "+subject); n != 1 {
		t.Fatalf("reply not delivered to pierre (%d)", n)
	}
	if n := countSubject(t, cfg.IMAP, "Sent", "RE: "+subject); n != 1 {
		t.Fatalf("sent copy missing (%d)", n)
	}
	if n := countSubject(t, cfg.IMAP, "Drafts", "RE: "+subject); n != 0 {
		t.Fatalf("draft not removed (%d)", n)
	}
}
