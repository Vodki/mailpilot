// Package smtptest is a recording SMTP server for tests (TLS or STARTTLS,
// AUTH PLAIN louis@example.test / secret).
package smtptest

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/testutil"
)

// Mail is one accepted message.
type Mail struct {
	From string
	To   []string
	Data []byte
}

// Server records MAIL FROM calls and messages.
type Server struct {
	mu        sync.Mutex
	mailCalls int
	mails     []Mail
	Config    config.SMTPConfig
}

// MailCalls returns how many MAIL FROM commands were received.
func (s *Server) MailCalls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.mailCalls }

// Mails returns the accepted messages.
func (s *Server) Mails() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mail(nil), s.mails...)
}

type backend struct{ s *Server }

func (b backend) NewSession(*smtp.Conn) (smtp.Session, error) { return &session{s: b.s}, nil }

type session struct {
	s    *Server
	from string
	to   []string
}

func (x *session) AuthMechanisms() []string { return []string{sasl.Plain} }
func (x *session) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user == "louis@example.test" && pass == "secret" {
			return nil
		}
		return errors.New("bad credentials")
	}), nil
}
func (x *session) Mail(from string, _ *smtp.MailOptions) error {
	x.s.mu.Lock()
	x.s.mailCalls++
	x.s.mu.Unlock()
	x.from = from
	return nil
}
func (x *session) Rcpt(to string, _ *smtp.RcptOptions) error { x.to = append(x.to, to); return nil }
func (x *session) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	x.s.mu.Lock()
	x.s.mails = append(x.s.mails, Mail{From: x.from, To: x.to, Data: b})
	x.s.mu.Unlock()
	return nil
}
func (x *session) Reset()        { x.from, x.to = "", nil }
func (x *session) Logout() error { return nil }

// Start runs a server; startTLS selects STARTTLS instead of implicit TLS.
// Config is ready to use, with send_enabled false.
func Start(t *testing.T, startTLS bool) *Server {
	t.Helper()
	s := &Server{}
	srv := smtp.NewServer(backend{s})
	srv.Domain = "localhost"
	tlsCfg := testutil.SelfSignedTLS(t)
	var ln net.Listener
	var err error
	ln, err = (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err == nil {
		if startTLS {
			srv.TLSConfig = tlsCfg
		} else {
			ln = tls.NewListener(ln, tlsCfg)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	host, port := testutil.SplitAddr(t, ln.Addr().String())
	sec := config.SecurityTLS
	if startTLS {
		sec = config.SecurityStartTLS
	}
	s.Config = config.SMTPConfig{
		Host: host, Port: port, Security: sec, Username: "louis@example.test",
		Password: config.NewSecret("secret"), InsecureSkipVerify: true, Timeout: 5 * time.Second, MaxSendsPerHour: 10,
	}
	return s
}
