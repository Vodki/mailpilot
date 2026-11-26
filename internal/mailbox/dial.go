// Package mailbox holds short-lived IMAP connections used for one-off
// commands (probe, fetch, APPEND, explicit \Seen). It never sets a flag on
// its own: every mutating method is called only from a user action.
package mailbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Vodki/mailpilot/internal/config"
)

// Dial opens an authenticated-ready (not yet logged in) IMAP connection
// honouring ctx for the TCP connect and TLS handshake.
func Dial(ctx context.Context, cfg config.IMAPConfig, opts *imapclient.Options) (*imapclient.Client, error) {
	if opts == nil {
		opts = &imapclient.Options{}
	}
	tlsCfg := &tls.Config{
		ServerName: cfg.Host,
		MinVersion: tls.VersionTLS12,
		// Test servers only; config validation keeps it false by default and
		// the check command prints a warning when it is set.
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // opt-in, test servers only
	}
	opts.TLSConfig = tlsCfg

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", cfg.Address())
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.Address(), err)
	}
	// Bound the handshake/greeting by ctx and timeout; cleared once ready.
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = raw.SetDeadline(deadline)

	var c *imapclient.Client
	switch cfg.Security {
	case config.SecurityTLS:
		tc := tls.Client(raw, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		c = imapclient.New(tc, opts)
	case config.SecurityStartTLS:
		c, err = imapclient.NewStartTLS(raw, opts)
		if err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	default:
		_ = raw.Close()
		return nil, fmt.Errorf("unsupported security %q", cfg.Security)
	}
	if err := c.WaitGreeting(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	_ = raw.SetDeadline(time.Time{})
	return c, nil
}
