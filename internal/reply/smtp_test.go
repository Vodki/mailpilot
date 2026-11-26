package reply

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/testutil"
)

type fakeBackend struct{ mailCalls atomic.Int32 }

func (b *fakeBackend) NewSession(*smtp.Conn) (smtp.Session, error) { return &fakeSession{b: b}, nil }

type fakeSession struct{ b *fakeBackend }

func (s *fakeSession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *fakeSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user == "louis@example.test" && pass == "secret" {
			return nil
		}
		return errors.New("bad credentials")
	}), nil
}
func (s *fakeSession) Mail(string, *smtp.MailOptions) error { s.b.mailCalls.Add(1); return nil }
func (s *fakeSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *fakeSession) Data(r io.Reader) error               { _, err := io.Copy(io.Discard, r); return err }
func (s *fakeSession) Reset()                               {}
func (s *fakeSession) Logout() error                        { return nil }

func startSMTP(t *testing.T, startTLS bool) (*fakeBackend, config.SMTPConfig) {
	t.Helper()
	be := &fakeBackend{}
	srv := smtp.NewServer(be)
	srv.Domain = "localhost"
	tlsCfg := testutil.SelfSignedTLS(t)
	var addr string
	if startTLS {
		srv.TLSConfig = tlsCfg
		srv.Addr = "127.0.0.1:0"
		ln, err := listenTCP()
		if err != nil {
			t.Fatal(err)
		}
		addr = ln.Addr().String()
		go func() { _ = srv.Serve(ln) }()
	} else {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
		if err != nil {
			t.Fatal(err)
		}
		addr = ln.Addr().String()
		go func() { _ = srv.Serve(ln) }()
	}
	t.Cleanup(func() { _ = srv.Close() })
	host, port := testutil.SplitAddr(t, addr)
	sec := config.SecurityTLS
	if startTLS {
		sec = config.SecurityStartTLS
	}
	return be, config.SMTPConfig{
		Host: host, Port: port, Security: sec, Username: "louis@example.test",
		Password: config.NewSecret("secret"), InsecureSkipVerify: true, Timeout: 5 * time.Second,
	}
}

func TestProbeSMTP(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		name := "implicit-tls"
		if startTLS {
			name = "starttls"
		}
		t.Run(name, func(t *testing.T) {
			be, cfg := startSMTP(t, startTLS)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := ProbeSMTP(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !res.TLS {
				t.Error("expected TLS")
			}
			if n := be.mailCalls.Load(); n != 0 {
				t.Fatalf("probe issued MAIL FROM %d times; it must never send", n)
			}
		})
	}
}

func TestProbeSMTPBadPassword(t *testing.T) {
	_, cfg := startSMTP(t, false)
	cfg.Password = config.NewSecret("nope")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ProbeSMTP(ctx, cfg); err == nil {
		t.Fatal("expected auth failure")
	}
}
