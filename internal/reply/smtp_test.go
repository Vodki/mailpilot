package reply

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/reply/smtptest"
)

func TestProbeSMTP(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		name := "implicit-tls"
		if startTLS {
			name = "starttls"
		}
		t.Run(name, func(t *testing.T) {
			srv := smtptest.Start(t, startTLS)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := ProbeSMTP(ctx, srv.Config)
			if err != nil {
				t.Fatal(err)
			}
			if !res.TLS {
				t.Error("expected TLS")
			}
			if n := srv.MailCalls(); n != 0 {
				t.Fatalf("probe issued MAIL FROM %d times; it must never send", n)
			}
		})
	}
}

func TestProbeSMTPBadPassword(t *testing.T) {
	srv := smtptest.Start(t, false)
	cfg := srv.Config
	cfg.Password = config.NewSecret("nope")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ProbeSMTP(ctx, cfg); err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestSendRefusedWhenDisabled(t *testing.T) {
	srv := smtptest.Start(t, true)
	err := Send(context.Background(), srv.Config, "louis@example.test", []string{"a@b.test"}, []byte("x"))
	if err != ErrSendDisabled || srv.MailCalls() != 0 { //nolint:errorlint // sentinel
		t.Fatalf("got %v, mail calls %d", err, srv.MailCalls())
	}
}

func TestSend(t *testing.T) {
	srv := smtptest.Start(t, true)
	cfg := srv.Config
	cfg.SendEnabled = true
	if err := Send(context.Background(), cfg, "louis@example.test", []string{"a@b.test", "c@d.test"}, []byte("Subject: x\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	m := srv.Mails()
	if len(m) != 1 || m[0].From != "louis@example.test" || strings.Join(m[0].To, ",") != "a@b.test,c@d.test" {
		t.Fatalf("mails %+v", m)
	}
}
