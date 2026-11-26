package app

import (
	"context"
	"strconv"
	"strings"

	"github.com/Vodki/mailpilot/internal/imapwatch"
	"github.com/Vodki/mailpilot/internal/parse"
	"github.com/Vodki/mailpilot/internal/store"
)

// Sink adapts the app to imapwatch.Sink.
type Sink struct{ a *App }

// Sink returns the IMAP sink.
func (a *App) Sink() imapwatch.Sink { return Sink{a} }

// LoadCursor implements imapwatch.Sink.
func (s Sink) LoadCursor(ctx context.Context, mailbox string) (imapwatch.Cursor, error) {
	st, _, err := s.a.st.GetMailboxState(ctx, mailbox)
	return imapwatch.Cursor{UIDValidity: st.UIDValidity, LastUID: st.LastUID, LastOK: st.LastOK, BootstrapDone: st.BootstrapDone}, err
}

// SaveCursor implements imapwatch.Sink.
func (s Sink) SaveCursor(ctx context.Context, mailbox string, c imapwatch.Cursor) error {
	return s.a.st.SaveMailboxState(ctx, store.MailboxState{
		Mailbox: mailbox, UIDValidity: c.UIDValidity, LastUID: c.LastUID, LastOK: c.LastOK, BootstrapDone: c.BootstrapDone,
	})
}

// Deliver parses and stores one message, then queues it for analysis.
func (s Sink) Deliver(ctx context.Context, f *imapwatch.Fetched) error {
	a := s.a
	p, err := parse.Parse(f.Raw)
	if err != nil {
		// Unparseable: keep a placeholder so Louis still hears about it.
		a.log.Warn("parse failed", "uid", f.UID, "err", err)
		p = &parse.Message{Subject: "(message illisible)"}
		p.MessageID = "unparsed-" + itoa(int64(f.UIDValidity)) + "-" + itoa(int64(f.UID)) + "@mailpilot.invalid"
		p.SyntheticID = true
	}
	m := a.toStore(p, f)
	if f.Silent {
		m.State, m.Delivery = store.StateSkipped, store.DeliverySilent
	}
	id, inserted, err := a.st.InsertMessage(ctx, m, p.ThreadKeys(), parse.NormalizeSubject(p.Subject))
	if err != nil {
		return err
	}
	a.log.Info("message stored", "message_pk", id, "uid", f.UID, "inserted", inserted, "silent", f.Silent,
		"sha256", short(p.BodySHA256), "size", f.Size)
	if inserted {
		a.afterInsert(ctx, id, m)
		if !f.Silent {
			a.Enqueue(id)
		}
	}
	return nil
}

func (a *App) toStore(p *parse.Message, f *imapwatch.Fetched) *store.Message {
	conv := func(in []parse.Address) []store.Address {
		out := make([]store.Address, 0, len(in))
		for _, x := range in {
			out = append(out, store.Address{Name: x.Name, Email: x.Email})
		}
		return out
	}
	m := &store.Message{
		MessageID: p.MessageID, SyntheticID: p.SyntheticID,
		Mailbox: f.Mailbox, UIDValidity: f.UIDValidity, UID: f.UID,
		From:    store.Address{Name: p.From.Name, Email: p.From.Email},
		ReplyTo: conv(p.ReplyTo), To: conv(p.To), Cc: conv(p.Cc),
		Subject: p.Subject, SentAt: p.Date, ReceivedAt: f.InternalDate,
		References: p.References, ListID: p.ListID, AutoSubmitted: p.AutoSubmitted,
		HasAttachments: p.HasRealAttachment(), Size: f.Size, BodySHA256: p.BodySHA256,
		TextClean: p.TextClean, TextFull: p.TextFull, HTMLOnly: p.HTMLOnly,
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = a.now()
	}
	if len(p.InReplyTo) > 0 {
		m.InReplyTo = p.InReplyTo[0]
	}
	for _, at := range p.Attachments {
		m.Attachments = append(m.Attachments, store.Attachment{Filename: at.Filename, MIMEType: at.MIMEType, Size: at.Size, Inline: at.Inline})
	}
	for _, t := range p.To {
		if a.self[strings.ToLower(t.Email)] {
			m.IsDirect = true
		}
	}
	if f.HeadersOnly {
		m.TextFull = "(message trop volumineux : seuls les en-têtes ont été lus)"
		m.TextClean = m.TextFull
	}
	return m
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
