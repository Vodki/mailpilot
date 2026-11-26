package reply

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/emersion/go-sasl"

	"github.com/Vodki/mailpilot/internal/config"
)

// ErrSendDisabled is returned whenever smtp.send_enabled is false. It is
// checked here as well as in the caller (defence in depth).
var ErrSendDisabled = errors.New("envoi désactivé (smtp.send_enabled: false)")

// Send submits one message over an encrypted, authenticated connection.
// The caller is responsible for Louis's double confirmation; this function
// only refuses when sending is disabled or the connection is not encrypted.
func Send(ctx context.Context, cfg config.SMTPConfig, envelopeFrom string, rcpts []string, raw []byte) error {
	if !cfg.SendEnabled {
		return ErrSendDisabled
	}
	if len(rcpts) == 0 {
		return ErrNoRecipient
	}
	c, err := dialSMTP(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("ehlo: %w", err)
	}
	if _, ok := c.TLSConnectionState(); !ok {
		return errors.New("connection is not encrypted, refusing to send")
	}
	if err := c.Auth(sasl.NewPlainClient("", cfg.Username, cfg.Password.Reveal())); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.SendMail(envelopeFrom, rcpts, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	_ = c.Quit()
	return nil
}
