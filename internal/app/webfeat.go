package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
