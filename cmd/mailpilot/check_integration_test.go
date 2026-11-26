//go:build integration

package main

// Integration tests against GreenMail (deploy/docker-compose.test.yml).
// Run with: make test-integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/mailbox"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func testConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := strings.Join([]string{
		"data_dir: " + dir,
		"imap:",
		"  host: " + env("MAILPILOT_TEST_HOST", "localhost"),
		"  port: " + env("MAILPILOT_TEST_IMAPS_PORT", "3993"),
		"  security: tls",
		"  username: louis@example.test",
		"  insecure_skip_verify: true",
		"smtp:",
		"  host: " + env("MAILPILOT_TEST_HOST", "localhost"),
		"  port: " + env("MAILPILOT_TEST_SMTPS_PORT", "3465"),
		"  security: tls",
		"  insecure_skip_verify: true",
	}, "\n")
	waitForPort(t, env("MAILPILOT_TEST_HOST", "localhost")+":"+env("MAILPILOT_TEST_IMAPS_PORT", "3993"))
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvIMAPPassword, "secret")
	return p
}

// waitForPort waits up to 60s for the test server to accept connections.
func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			time.Sleep(time.Second) // let GreenMail finish starting its services
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("test server %s not reachable: %v", addr, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// deliver drops an unread message into the test mailbox via SMTPS.
func deliver(t *testing.T, cfg *config.Config) {
	t.Helper()
	c, err := smtp.DialTLS(cfg.SMTP.Address(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Auth(sasl.NewPlainClient("", "louis@example.test", "secret")); err != nil {
		t.Fatal(err)
	}
	msg := "From: pierre@example.test\r\nTo: louis@example.test\r\nSubject: Test phase 0\r\n" +
		"Message-ID: <phase0-" + time.Now().Format("150405.000") + "@example.test>\r\n\r\nBonjour\r\n"
	if err := c.SendMail("pierre@example.test", []string{"louis@example.test"}, strings.NewReader(msg)); err != nil {
		t.Fatal(err)
	}
}

func unseenCount(t *testing.T, cfg *config.Config) uint32 {
	t.Helper()
	c, err := mailbox.Dial(context.Background(), cfg.IMAP, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(cfg.IMAP.Username, cfg.IMAP.Password.Reveal()).Wait(); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status("INBOX", &imap.StatusOptions{NumUnseen: true, NumMessages: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return *st.NumUnseen
}

func TestCheckAgainstGreenMail(t *testing.T) {
	path := testConfig(t)
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, cfg)
	time.Sleep(500 * time.Millisecond)
	before := unseenCount(t, cfg)
	if before == 0 {
		t.Fatal("test message not delivered")
	}

	var out, errb bytes.Buffer
	code := run([]string{"check", "-config", path, "-only", "store,imap,smtp"}, &out, &errb)
	t.Log("\n" + out.String())
	if code != 0 {
		t.Fatalf("check failed (code %d): %s", code, errb.String())
	}
	for _, want := range []string{"CAPABILITY après login", "IDLE : oui", "UIDVALIDITY", "aucun message envoyé"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}

	if after := unseenCount(t, cfg); after != before {
		t.Fatalf("check changed \\Seen flags: unseen %d -> %d", before, after)
	}
}
