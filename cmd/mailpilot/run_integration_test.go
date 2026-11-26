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
