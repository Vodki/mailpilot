package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Vodki/mailpilot/internal/mailbox"
	"github.com/Vodki/mailpilot/internal/reply"
	"github.com/Vodki/mailpilot/internal/store"
)

// The web interface reaches drafts, sends and reminders only through these
// methods (cmd/mailpilot adapts them to web.Composer). They run the same
// checks as the Telegram buttons: same gates, same recipient rules, same
// single-send transition.

// Errors shown as-is to Louis by the web interface.
var (
	ErrWebPaused     = errors.New("mailpilot est en pause : /resume d'abord")
	ErrWebSuspect    = errors.New("mail suspect : pas de brouillon rédigé par l'IA, écris ta réponse toi-même")
	ErrWebNoDraft    = errors.New("ce brouillon n'est plus le brouillon en cours")
	ErrWebRecipients = errors.New("destinataires incohérents : envoi refusé")
)

// WebDraftRequest asks for a new version of the reply to a message.
type WebDraftRequest struct {
	MessagePK    int64
	Instructions string // added to the previous instructions when Regenerate
	Body         string // Louis's own text: stored as is, no LLM
	ReplyAll     bool
	Regenerate   bool // rewrite the current draft following Instructions
}

// WebDraft writes a reply draft and stores it in Drafts, synchronously.
func (a *App) WebDraft(ctx context.Context, r WebDraftRequest) (*store.Draft, error) {
	m, err := a.st.GetMessage(ctx, r.MessagePK)
	if err != nil {
		return nil, err
	}
	req := draftRequest{m: m, literal: strings.TrimSpace(r.Body), replyAll: r.ReplyAll, quiet: true,
		instructions: strings.TrimSpace(r.Instructions)}
	if req.literal == "" {
		if a.Paused() {
			return nil, ErrWebPaused
		}
		if t := a.latestTriage(ctx, m.ID); t != nil && (t.Category == "spam_probable" || t.AIInstructions()) {
			return nil, ErrWebSuspect
		}
	}
	if prev, err := a.st.ActiveDraft(ctx, m.ID); err == nil {
		if r.Regenerate {
			req.previous = prev.Body
			req.instructions = strings.TrimSpace(prev.Instructions + "\n" + req.instructions)
		} else if req.instructions == "" {
			req.instructions = prev.Instructions
		}
	}
	d, err := a.makeDraft(ctx, req)
	if err != nil {
		return nil, err
	}
	a.audit(ctx, "web", "draft", fmt.Sprintf("message=%d draft=%d v=%d", m.ID, d.ID, d.Version))
	return d, nil
}

// WebCancelDraft drops the active draft and removes it from Drafts.
func (a *App) WebCancelDraft(ctx context.Context, draftID int64) error {
	d, err := a.st.GetDraft(ctx, draftID)
	if err != nil {
		return err
	}
	ok, err := a.st.TransitionDraft(ctx, d.ID, store.DraftActive, store.DraftCancelled, "", a.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrWebNoDraft
	}
	a.retireDraft(ctx, d)
	a.audit(ctx, "web", "draft_cancel", fmt.Sprintf("draft=%d", d.ID))
	return nil
}

// WebSendGate returns why sending is refused right now ("" if allowed).
func (a *App) WebSendGate(ctx context.Context) string { return a.sendGate(ctx) }

// WebSend sends a draft Louis confirmed twice. version must still be the
// active version: an edit after the confirmation cancels it.
func (a *App) WebSend(ctx context.Context, draftID int64, version int) (string, error) {
	if why := a.sendGate(ctx); why != "" {
		return "", errors.New(why)
	}
	d, err := a.st.GetDraft(ctx, draftID)
	if err != nil {
		return "", err
	}
	if d.Status != store.DraftActive || d.Version != version {
		return "", ErrWebNoDraft
	}
	m, err := a.st.GetMessage(ctx, d.MessagePK)
	if err != nil {
		return "", err
	}
	note, err := a.sendDraft(ctx, d, m, "web", nil)
	switch {
	case errors.Is(err, errRecipients):
		return "", ErrWebRecipients
	case errors.Is(err, errAlreadyHandled):
		return "", ErrWebNoDraft
	}
	return note, err
}

// WebTodo creates (add) or dismisses a proposed reminder.
func (a *App) WebTodo(ctx context.Context, actionID int64, add bool) error {
	act, err := a.st.GetAction(ctx, actionID)
	if err != nil {
		return err
	}
	if !add {
		if act.Status != store.ActionProposed && act.Status != store.ActionFailed {
			return errAlreadyHandled
		}
		return a.st.SetActionStatus(ctx, act.ID, store.ActionDismissed, "", "")
	}
	if a.Paused() {
		return ErrWebPaused
	}
	if a.todo == nil {
		return errors.New("rappels Todoist non configurés")
	}
	return a.createTask(ctx, actionID, store.ActionProposed, store.ActionFailed)
}

// WebComposeRequest saves a new message or a forward. Recipients are what
// Louis typed; they are parsed strictly and never come from a mail.
type WebComposeRequest struct {
	ID      int64 // 0: a new composition
	Forward int64 // forwarded message (new composition only)
	To, Cc  string
	Subject string
	Body    string
}

// WebCompose stores a version of a new message or forward, and its copy in
// Drafts (the previous copy is removed).
func (a *App) WebCompose(ctx context.Context, r WebComposeRequest) (*store.Composition, error) {
	if a.ops == nil {
		return nil, errors.New("brouillons non configurés")
	}
	to, err := reply.ParseRecipients(r.To)
	if err != nil {
		return nil, err
	}
	if len(to) == 0 {
		return nil, errors.New("indique au moins un destinataire")
	}
	cc, err := reply.ParseRecipients(r.Cc)
	if err != nil {
		return nil, err
	}
	if len(to)+len(cc) > reply.MaxRecipients {
		return nil, fmt.Errorf("trop de destinataires (%d au plus)", reply.MaxRecipients)
	}
	c := &store.Composition{ID: r.ID, Kind: store.CompositionNew, To: toStoreAddrs(to), Cc: toStoreAddrs(cc),
		Subject: strings.TrimSpace(r.Subject), Body: strings.TrimSpace(r.Body)}
	source := r.Forward
	if r.ID != 0 {
		cur, err := a.st.GetComposition(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		if cur.Status != "active" {
			return nil, ErrWebNoDraft
		}
		source = cur.SourcePK
	}
	var built *reply.Draft
	if source != 0 {
		m, err := a.st.GetMessage(ctx, source)
		if err != nil {
			return nil, err
		}
		o := original(m)
		o.QuoteText = m.TextFull
		if o.QuoteText == "" {
			o.QuoteText = m.TextClean
		}
		c.Kind, c.SourcePK = store.CompositionForward, source
		built, err = reply.BuildForward(o, a.fromAddr(), to, cc, c.Subject, c.Body, a.now(), a.cfg.Location)
		if err != nil {
			return nil, err
		}
	} else {
		built, err = reply.BuildNew(a.fromAddr(), to, cc, c.Subject, c.Body, a.now(), a.cfg.Location)
		if err != nil {
			return nil, err
		}
	}
	c.Subject, c.RFCMessageID, c.Raw = built.Subject, built.MessageID, built.Raw
	prev, err := a.st.SaveComposition(ctx, c)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrWebNoDraft
		}
		return nil, err
	}
	if prev != nil {
		a.retireComposition(ctx, prev)
	}
	if st, err := a.ops.AppendDraft(ctx, c.Raw); err != nil {
		a.log.Warn("composition append failed", "composition_id", c.ID, "err", err)
	} else {
		_ = a.st.SetCompositionIMAP(ctx, c.ID, st.Folder, st.UIDValidity, st.UID)
		c.IMAPFolder, c.IMAPUIDValidity, c.IMAPUID = st.Folder, st.UIDValidity, st.UID
	}
	a.audit(ctx, "web", "compose", fmt.Sprintf("composition=%d kind=%s v=%d rcpts=%d", c.ID, c.Kind, c.Version, len(to)+len(cc)))
	return c, nil
}

func (a *App) retireComposition(ctx context.Context, c *store.Composition) {
	if c.IMAPFolder == "" || a.ops == nil {
		return
	}
	if err := a.ops.Delete(ctx, mailbox.Stored{Folder: c.IMAPFolder, UIDValidity: c.IMAPUIDValidity, UID: c.IMAPUID}, c.RFCMessageID); err != nil {
		a.log.Warn("composition delete failed", "composition_id", c.ID, "err", err)
	}
}

// WebCancelComposition drops a composition and its copy in Drafts.
func (a *App) WebCancelComposition(ctx context.Context, id int64) error {
	c, err := a.st.GetComposition(ctx, id)
	if err != nil {
		return err
	}
	ok, err := a.st.TransitionComposition(ctx, id, "active", "cancelled", "", a.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrWebNoDraft
	}
	a.retireComposition(ctx, c)
	a.audit(ctx, "web", "compose_cancel", fmt.Sprintf("composition=%d", id))
	return nil
}

// WebSendComposition sends a composition Louis confirmed twice, if version
// is still the current one. Same gates as a reply.
func (a *App) WebSendComposition(ctx context.Context, id int64, version int) (string, error) {
	if why := a.sendGate(ctx); why != "" {
		return "", errors.New(why)
	}
	c, err := a.st.GetComposition(ctx, id)
	if err != nil {
		return "", err
	}
	if c.Status != "active" || c.Version != version || len(c.To) == 0 {
		return "", ErrWebNoDraft
	}
	if ok, err := a.st.TransitionComposition(ctx, id, "active", "sending", "", a.now()); err != nil || !ok {
		return "", ErrWebNoDraft
	}
	var rcpts []string
	for _, l := range [][]store.Address{c.To, c.Cc} {
		for _, x := range l {
			rcpts = append(rcpts, x.Email)
		}
	}
	a.audit(ctx, "web", "send_confirmed", fmt.Sprintf("composition=%d rcpts=%d", id, len(rcpts)))
	if err := a.sendMail(ctx, a.cfg.SMTP, a.fromAddr().Email, rcpts, c.Raw); err != nil {
		_, _ = a.st.TransitionComposition(ctx, id, "sending", "active", truncateRunes(err.Error(), 500), a.now())
		a.audit(ctx, "mailpilot", "send_failed", fmt.Sprintf("composition=%d", id))
		a.log.Warn("smtp send failed", "composition_id", id, "err", err)
		return "", err
	}
	_, _ = a.st.TransitionComposition(ctx, id, "sending", "sent", "", a.now())
	a.audit(ctx, "mailpilot", "sent", fmt.Sprintf("composition=%d", id))
	a.log.Info("message sent", "composition_id", id, "kind", c.Kind, "rcpts", len(rcpts))
	note := ""
	if a.cfg.IMAP.SaveSent {
		if _, err := a.ops.AppendSent(ctx, c.Raw); err != nil {
			note = "Copie dans Envoyés impossible : " + truncateRunes(err.Error(), 150)
		}
	}
	a.retireComposition(ctx, c)
	return note, nil
}
