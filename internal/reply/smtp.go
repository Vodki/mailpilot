// Package reply owns everything that can lead to an outgoing email. It is the
// only package allowed to import the SMTP client (enforced by
// internal/archtest). It computes reply recipients from headers, builds the
// message and submits it, only after the caller's double confirmation.
package reply

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/Vodki/mailpilot/internal/config"
)

// SMTPProbeResult is what `mailpilot check` reports about the SMTP server.
type SMTPProbeResult struct {
	AuthMechanisms string
	TLS            bool
}

// ProbeSMTP connects with TLS or STARTTLS, authenticates with AUTH PLAIN and
// quits. It never issues MAIL FROM, so nothing can be sent.
func ProbeSMTP(ctx context.Context, cfg config.SMTPConfig) (*SMTPProbeResult, error) {
	c, err := dialSMTP(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	if err := c.Hello("localhost"); err != nil {
		return nil, fmt.Errorf("ehlo: %w", err)
	}
	res := &SMTPProbeResult{}
	_, res.TLS = c.TLSConnectionState()
	if !res.TLS {
		return nil, errors.New("connection is not encrypted, refusing to authenticate")
	}
	_, res.AuthMechanisms = c.Extension("AUTH")
	if !c.SupportsAuth(sasl.Plain) {
		return res, fmt.Errorf("server does not offer AUTH PLAIN (offers %q)", res.AuthMechanisms)
	}
	if err := c.Auth(sasl.NewPlainClient("", cfg.Username, cfg.Password.Reveal())); err != nil {
		return res, fmt.Errorf("auth: %w", err)
	}
	if err := c.Quit(); err != nil {
		return res, fmt.Errorf("quit: %w", err)
	}
	return res, nil
}

func dialSMTP(ctx context.Context, cfg config.SMTPConfig) (*smtp.Client, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	tlsCfg := &tls.Config{
		ServerName:         cfg.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // opt-in, test servers only
	}
	d := net.Dialer{Timeout: timeout}
	raw, err := d.DialContext(ctx, "tcp", cfg.Address())
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.Address(), err)
	}
	_ = raw.SetDeadline(time.Now().Add(timeout))

	var c *smtp.Client
	switch cfg.Security {
	case config.SecurityTLS:
		tc := tls.Client(raw, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		c = smtp.NewClient(tc)
	case config.SecurityStartTLS:
		c, err = smtp.NewClientStartTLS(raw, tlsCfg)
		if err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	default:
		_ = raw.Close()
		return nil, fmt.Errorf("unsupported security %q", cfg.Security)
	}
	c.CommandTimeout = timeout
	return c, nil
}
