package mailbox

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/testutil"
)

// startServer runs an in-memory IMAP server over implicit TLS.
func startServer(t *testing.T, caps imap.CapSet) string {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("louis@example.test", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         caps,
		InsecureAuth: true, // the listener below is already TLS
	})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", testutil.SelfSignedTLS(t))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func cfgFor(t *testing.T, addr, password string) config.IMAPConfig {
	host, port := testutil.SplitAddr(t, addr)
	return config.IMAPConfig{
		Host: host, Port: port, Security: config.SecurityTLS,
		Username: "louis@example.test", Password: config.NewSecret(password),
		Mailbox: "INBOX", InsecureSkipVerify: true, Timeout: 5 * time.Second,
	}
}

func TestProbe(t *testing.T) {
	addr := startServer(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Probe(ctx, cfgFor(t, addr, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasIdle || res.HasSpecialUse {
		t.Errorf("idle=%v specialuse=%v", res.HasIdle, res.HasSpecialUse)
	}
	if !slices.Contains(res.CapsAfterLogin, "IDLE") {
		t.Errorf("caps after login: %v", res.CapsAfterLogin)
	}
	if res.UIDValidity == 0 {
		t.Errorf("UIDVALIDITY not read")
	}
}

func TestProbeBadPassword(t *testing.T) {
	addr := startServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Probe(ctx, cfgFor(t, addr, "wrong")); err == nil {
		t.Fatal("expected login failure")
	}
}

func TestProbeRejectsUntrustedCert(t *testing.T) {
	addr := startServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	cfg := cfgFor(t, addr, "secret")
	cfg.InsecureSkipVerify = false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Probe(ctx, cfg); err == nil {
		t.Fatal("self-signed certificate must be rejected by default")
	}
}

func TestDialHonoursContext(t *testing.T) {
	// A listener that accepts but never speaks: the greeting must time out.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
		}
	}()
	cfg := cfgFor(t, ln.Addr().String(), "x")
	cfg.Timeout = 300 * time.Millisecond
	start := time.Now()
	if _, err := Dial(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("dial did not honour timeout: %s", time.Since(start))
	}
}
