package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) Env {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndSecrets(t *testing.T) {
	path := writeFile(t, "config.yaml", "imap:\n  username: moi@example.com\n")
	secretFile := writeFile(t, "tg", "tok-123\n")
	cfg, err := Load(path, envMap(map[string]string{
		EnvIMAPPassword:            "pw",
		EnvTelegramToken + "_FILE": secretFile,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP.Host != "pro3.mail.ovh.net" || cfg.IMAP.Port != 993 || cfg.IMAP.Security != SecurityTLS {
		t.Errorf("unexpected IMAP defaults: %+v", cfg.IMAP)
	}
	if cfg.SMTP.Port != 587 || cfg.SMTP.Security != SecurityStartTLS || cfg.SMTP.SendEnabled {
		t.Errorf("unexpected SMTP defaults: %+v", cfg.SMTP)
	}
	if cfg.SMTP.Username != "moi@example.com" || cfg.SMTP.Password.Reveal() != "pw" {
		t.Errorf("SMTP credentials should default to IMAP ones")
	}
	if cfg.Telegram.Token.Reveal() != "tok-123" {
		t.Errorf("token from _FILE not trimmed: %q", cfg.Telegram.Token.Reveal())
	}
	if cfg.Location == nil || cfg.Location.String() != "Europe/Paris" {
		t.Errorf("location not resolved")
	}
	if cfg.IMAP.IdleRefresh != 25*time.Minute {
		t.Errorf("idle refresh default = %s", cfg.IMAP.IdleRefresh)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeFile(t, "config.yaml", "imap:\n  username: a@b.c\n  pasword: oops\n")
	_, err := Load(path, envMap(nil))
	if err == nil || !strings.Contains(err.Error(), "pasword") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]string{
		"idle too long":      "imap: {username: a@b.c, idle_refresh: 30m}",
		"plaintext imap":     "imap: {username: a@b.c, security: none}",
		"missing username":   "imap: {host: x}",
		"bad digest time":    "imap: {username: a@b.c}\ndigest: {times: ['8h30']}",
		"bad provider":       "imap: {username: a@b.c}\nllm: {provider: openai}",
		"bad todoist mode":   "imap: {username: a@b.c}\ntodoist: {mode_dated: maybe}",
		"bad timezone":       "imap: {username: a@b.c}\ntimezone: Mars/Olympus",
		"zero retention":     "imap: {username: a@b.c}\nretention: {body_days: 0}",
		"plaintext smtp":     "imap: {username: a@b.c}\nsmtp: {security: plain}",
		"bad effort":         "imap: {username: a@b.c}\nllm: {tasks: {triage: {model: m, effort: huge}, draft: {model: m}, ask: {model: m}}}",
		"missing task":       "imap: {username: a@b.c}\nllm: {tasks: {triage: {model: m}}}",
		"short poll":         "imap: {username: a@b.c, poll_interval: 1s}",
		"negative bootstrap": "imap: {username: a@b.c, bootstrap_days: -1}",
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeFile(t, "c.yaml", yml), envMap(nil)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSecretNeverPrinted(t *testing.T) {
	s := NewSecret("hunter2")
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "pw", s)
	js, _ := json.Marshal(struct{ P Secret }{s})
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s), buf.String(), string(js)} {
		if strings.Contains(out, "hunter2") {
			t.Fatalf("secret leaked: %s", out)
		}
	}
	if s.Reveal() != "hunter2" {
		t.Fatal("Reveal broken")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../config.example/config.yaml", envMap(nil))
	if err != nil {
		t.Fatalf("config.example/config.yaml must stay valid: %v", err)
	}
	if cfg.SMTP.SendEnabled {
		t.Fatal("example config must keep send_enabled: false")
	}
}

func TestWebConfig(t *testing.T) {
	const hash = "$argon2id$v=19$m=65536,t=2,p=1$c2FsdA$aGFzaA"
	cases := []struct {
		name, yaml, hash, wantErr string
	}{
		{"off by default", "", "", ""},
		{"loopback ok", "web:\n  enabled: true\n", hash, ""},
		{"no hash", "web:\n  enabled: true\n", "", "MAILPILOT_WEB_PASSWORD_HASH is empty"},
		{"not argon2", "web:\n  enabled: true\n", "$2a$10$x", "must be an argon2id hash"},
		{"public refused", "web:\n  enabled: true\n  listen: 0.0.0.0:8484\n", hash, "not a loopback address"},
		{"public allowed", "web:\n  enabled: true\n  listen: 0.0.0.0:8484\n  allow_remote: true\n  public_url: https://mail.example.com\n", hash, ""},
		{"public needs https", "web:\n  enabled: true\n  listen: 0.0.0.0:8484\n  allow_remote: true\n  public_url: http://mail.example.com\n", hash, "must use https"},
		{"bad listen", "web:\n  enabled: true\n  listen: nope\n", hash, "must be host:port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeFile(t, "config.yaml", "imap:\n  username: moi@example.com\n"+c.yaml)
			env := map[string]string{}
			if c.hash != "" {
				env[EnvWebPassword] = c.hash
			}
			_, err := Load(path, envMap(env))
			if c.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("want %q, got %v", c.wantErr, err)
			}
		})
	}
}
