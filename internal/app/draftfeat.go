package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/mailbox"
	"github.com/Vodki/mailpilot/internal/reply"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

// MailOps is the IMAP write surface (interface for tests).
type MailOps interface {
	AppendDraft(ctx context.Context, raw []byte) (*mailbox.Stored, error)
	AppendSent(ctx context.Context, raw []byte) (*mailbox.Stored, error)
	Delete(ctx context.Context, s mailbox.Stored, messageID string) error
	MarkSeen(ctx context.Context, mailbox string, uidValidity, uid uint32) error
}

// SendFunc submits a message (reply.Send in production).
type SendFunc func(ctx context.Context, cfg config.SMTPConfig, envelopeFrom string, rcpts []string, raw []byte) error

// Callback actions for drafts and message buttons.
const (
	cbDraftNew     = "draft_new"
	cbDraftEdit    = "draft_edit"
	cbDraftRegen   = "draft_regen"
	cbDraftAll     = "draft_all"
	cbDraftCancel  = "draft_cancel"
	cbDraftSend    = "draft_send"
	cbSendConfirm  = "send_confirm"
	cbSendAbort    = "send_abort"
	cbMarkRead     = "mark_read"
	cbThreadIgnore = "thread_ignore"

	sendConfirmTTL = 10 * time.Minute
	editWindow     = 30 * time.Minute
)

// draftState serialises draft work and remembers a pending "Modifier".
type draftState struct {
	mu         sync.Mutex // one draft operation at a time
	editMu     sync.Mutex
	editDraft  int64
	editExpiry time.Time
}

// original converts a stored message to reply headers. Addresses come only
// from the stored headers.
func original(m *store.Message) reply.Original {
	conv := func(in []store.Address) []reply.Addr {
		out := make([]reply.Addr, 0, len(in))
		for _, x := range in {
			out = append(out, reply.Addr{Name: x.Name, Email: x.Email})
		}
		return out
	}
	o := reply.Original{
		MessageID: m.MessageID, References: m.References, From: reply.Addr{Name: m.From.Name, Email: m.From.Email},
		ReplyTo: conv(m.ReplyTo), To: conv(m.To), Cc: conv(m.Cc), Subject: m.Subject, Date: m.SentAt, QuoteText: m.TextClean,
	}
	if o.Date.IsZero() {
		o.Date = m.ReceivedAt
	}
	if m.SyntheticID {
		o.MessageID = ""
	}
	return o
}

func toStoreAddrs(in []reply.Addr) []store.Address {
	out := make([]store.Address, 0, len(in))
	for _, x := range in {
		out = append(out, store.Address{Name: x.Name, Email: x.Email})
	}
	return out
}

func (a *App) fromAddr() reply.Addr {
	return reply.Addr{Name: a.cfg.SMTP.FromName, Email: strings.ToLower(a.cfg.IMAP.Username)}
}

// draftRequest describes a new draft version.
type draftRequest struct {
	m            *store.Message
	instructions string
	previous     string
	literal      string // Louis typed the full text himself: no LLM
	replyAll     bool
	quiet        bool // web interface: no Telegram message for this version
}

// startDraft runs makeDraft in the background (LLM calls take seconds).
func (a *App) startDraft(ctx context.Context, r draftRequest) {
	a.goRun(func() {
		if _, err := a.makeDraft(ctx, r); err != nil {
			a.log.Warn("draft failed", "message_pk", r.m.ID, "err", err)
			a.sendText(ctx, "⚠️ Brouillon impossible : "+truncateRunes(err.Error(), 500))
		}
	})
}

func (a *App) makeDraft(ctx context.Context, r draftRequest) (*store.Draft, error) {
	if a.ops == nil {
		return nil, errors.New("brouillons non configurés")
	}
	a.drafts.mu.Lock()
	defer a.drafts.mu.Unlock()
	m := r.m
	orig := original(m)
	to, cc := reply.Recipients(orig, a.self, r.replyAll)
	if len(to) == 0 {
		return nil, reply.ErrNoRecipient
	}
	body := strings.TrimSpace(r.literal)
	if body == "" {
		in := analysis.DraftInput{
			Email: emailOf(m), Profile: a.profile(), Instructions: r.instructions, Previous: r.previous,
			ReplyAll: r.replyAll, Recipients: reply.Emails(to, cc), Now: a.now(),
		}
		if m.ThreadID > 0 {
			prev, _ := a.st.ThreadMessages(ctx, m.ThreadID, 6)
			for _, p := range prev {
				if p.ID != m.ID && !p.ReceivedAt.After(m.ReceivedAt) {
					in.Thread = append(in.Thread, emailOf(p))
				}
			}
		}
		res, attempts, err := a.drafter.Load().Draft(ctx, in)
		a.recordCalls(ctx, "draft", m.ID, attempts)
		if err != nil {
			return nil, err
		}
		body = res.Body
	}
	built, err := reply.Build(orig, a.fromAddr(), to, cc, body, a.now(), a.cfg.Location)
	if err != nil {
		return nil, err
	}
	d := &store.Draft{MessagePK: m.ID, Body: body, Instructions: r.instructions, ReplyAll: r.replyAll,
		To: toStoreAddrs(to), Cc: toStoreAddrs(cc), Subject: built.Subject, RFCMessageID: built.MessageID, Raw: built.Raw}
	prev, err := a.st.NewDraftVersion(ctx, d)
	if err != nil {
		return nil, err
	}
	if prev != nil {
		a.retireDraft(ctx, prev)
	}
	imapNote := "Enregistré dans le dossier Brouillons."
	if st, err := a.ops.AppendDraft(ctx, built.Raw); err != nil {
		a.log.Warn("draft append failed", "draft_id", d.ID, "err", err)
		imapNote = "⚠️ Non enregistré dans Brouillons : " + truncateRunes(err.Error(), 200)
	} else {
		_ = a.st.SetDraftIMAP(ctx, d.ID, st.Folder, st.UIDValidity, st.UID)
		d.IMAPFolder, d.IMAPUIDValidity, d.IMAPUID = st.Folder, st.UIDValidity, st.UID
	}
	a.audit(ctx, "mailpilot", "draft", fmt.Sprintf("message=%d draft=%d v=%d", m.ID, d.ID, d.Version))
	if r.quiet {
		return d, nil
	}
	text, complete := a.formatDraft(m, d, imapNote)
	sent, err := a.send(ctx, text, telegram.SendOptions{Markup: a.draftKeyboard(ctx, d, complete)})
	if err != nil {
		return d, err
	}
	return d, a.st.SetDraftTelegram(ctx, d.ID, sent.MessageID)
}

// retireDraft removes a superseded or cancelled version from Drafts and
// strips its Telegram buttons.
func (a *App) retireDraft(ctx context.Context, d *store.Draft) {
	if d.IMAPFolder != "" && a.ops != nil {
		if err := a.ops.Delete(ctx, mailbox.Stored{Folder: d.IMAPFolder, UIDValidity: d.IMAPUIDValidity, UID: d.IMAPUID}, d.RFCMessageID); err != nil {
			a.log.Warn("draft delete failed", "draft_id", d.ID, "err", err)
		}
	}
	if d.TGMessageID != 0 {
		_ = a.tg.EditMarkup(ctx, a.cfg.Telegram.ChatID, d.TGMessageID, nil)
	}
}

func addrLine(l []store.Address) string {
	parts := make([]string, len(l))
	for i, x := range l {
		parts[i] = headerAddr(x)
	}
	return strings.Join(parts, ", ")
}

// formatDraft renders a draft; complete=false when the body did not fit
// (then sending from Telegram is refused: Louis must see what he sends).
func (a *App) formatDraft(m *store.Message, d *store.Draft, note string) (telegram.Text, bool) {
	var b telegram.Builder
	who := m.From.Name
	if who == "" {
		who = m.From.Email
	}
	b.Bold(fmt.Sprintf("✍️ Brouillon v%d", d.Version)).Plain(" · réponse à " + Defang(who) + "\n")
	b.Plain("À : " + addrLine(d.To) + "\n")
	if len(d.Cc) > 0 {
		b.Plain("Cc : " + addrLine(d.Cc) + "\n")
	}
	b.Plain("Objet : " + Defang(d.Subject) + "\n")
	if w := replyToWarning(m); w != "" {
		b.Plain(w + "\n")
	}
	b.Plain("────────\n")
	head := b.Len()
	b.Plain(d.Body + "\n")
	b.Plain("────────\n" + note)
	complete := b.Len() <= telegram.MaxText && head > 0
	if !complete {
		b = telegram.Builder{}
		b.Bold(fmt.Sprintf("✍️ Brouillon v%d", d.Version)).Plain(" · trop long pour être relu ici.\n")
		b.Plain("À : " + addrLine(d.To) + "\n" + note + "\nRelis-le et envoie-le depuis ta messagerie.")
	}
	return b.Text(), complete
}

// replyToWarning flags a Reply-To that points somewhere else than From.
func replyToWarning(m *store.Message) string {
	if len(m.ReplyTo) == 0 {
		return ""
	}
	for _, r := range m.ReplyTo {
		if !strings.EqualFold(r.Email, m.From.Email) {
			return "⚠️ La réponse part vers l'adresse Reply-To (" + r.Email + "), différente de l'expéditeur (" + m.From.Email + ")."
		}
	}
	return ""
}

func (a *App) button(ctx context.Context, label, action string, msgPK, draftPK int64, arg string, ttl time.Duration) (telegram.Button, bool) {
	tok, err := a.st.CreateCallback(ctx, action, msgPK, draftPK, arg, a.now().Add(ttl))
	if err != nil {
		a.log.Error("callback token", "err", err)
		return telegram.Button{}, false
	}
	return telegram.Button{Text: label, CallbackData: tok}, true
}

func row(btns ...telegram.Button) []telegram.Button {
	var out []telegram.Button
	for _, b := range btns {
		if b.CallbackData != "" {
			out = append(out, b)
		}
	}
	return out
}

func (a *App) draftKeyboard(ctx context.Context, d *store.Draft, complete bool) *telegram.InlineMarkup {
	v := strconv.Itoa(d.Version)
	btn := func(label, action string) telegram.Button {
		b, _ := a.button(ctx, label, action, d.MessagePK, d.ID, v, callbackTTL)
		return b
	}
	var rows [][]telegram.Button
	if complete {
		rows = append(rows, row(btn("✉️ Envoyer", cbDraftSend), btn("✏️ Modifier", cbDraftEdit)))
	} else {
		rows = append(rows, row(btn("✏️ Modifier", cbDraftEdit)))
	}
	all := "👥 Répondre à tous"
	if d.ReplyAll {
		all = "👤 Seulement l'expéditeur"
	}
	rows = append(rows, row(btn("🔄 Régénérer", cbDraftRegen), btn(all, cbDraftAll)), row(btn("❌ Annuler", cbDraftCancel)))
	return &telegram.InlineMarkup{InlineKeyboard: rows}
}

// messageButtons are the per-mail actions on a notification.
func (a *App) messageButtons(ctx context.Context, m *store.Message) [][]telegram.Button {
	if a.ops == nil {
		return nil
	}
	b1, _ := a.button(ctx, "✍️ Brouillon", cbDraftNew, m.ID, 0, "", callbackTTL)
	b2, _ := a.button(ctx, "👁 Marquer lu", cbMarkRead, m.ID, 0, "", callbackTTL)
	b3, _ := a.button(ctx, "🔕 Ignorer le fil", cbThreadIgnore, m.ID, 0, "", callbackTTL)
	return [][]telegram.Button{row(b1, b2, b3)}
}

// activeDraftFor loads the draft a button refers to and checks it is still
// the active version.
func (a *App) activeDraftFor(ctx context.Context, c *store.Callback) (*store.Draft, *store.Message, string) {
	d, err := a.st.GetDraft(ctx, c.DraftPK)
	if err != nil {
		return nil, nil, "Brouillon introuvable."
	}
	if d.Status != store.DraftActive || strconv.Itoa(d.Version) != c.Arg {
		return nil, nil, "Ce brouillon a été remplacé ou n'est plus actif."
	}
	m, err := a.st.GetMessage(ctx, d.MessagePK)
	if err != nil {
		return nil, nil, "Mail introuvable."
	}
	return d, m, ""
}

// draftCallback handles every draft and message button.
func (a *App) draftCallback(ctx context.Context, q *telegram.CallbackQuery, c *store.Callback) bool {
	answer := func(s string) { _ = a.tg.AnswerCallback(ctx, q.ID, s) }
	switch c.Action {
	case cbDraftNew:
		defer a.refreshFrom(ctx, q, c)
		if a.Paused() {
			answer("En pause : /resume d'abord.")
			return true
		}
		m, err := a.st.GetMessage(ctx, c.MessagePK)
		if err != nil {
			answer("Mail introuvable.")
			return true
		}
		answer("Rédaction du brouillon…")
		a.startDraft(ctx, draftRequest{m: m})
	case cbDraftEdit:
		d, _, why := a.activeDraftFor(ctx, c)
		if d == nil {
			answer(why)
			return true
		}
		a.drafts.editMu.Lock()
		a.drafts.editDraft, a.drafts.editExpiry = d.ID, a.now().Add(editWindow)
		a.drafts.editMu.Unlock()
		a.reissueDraftButton(ctx, c)
		answer("")
		a.sendText(ctx, "✏️ Écris tes consignes pour la version suivante (ex. « plus court, propose mardi 10 h »).\nOu commence ton message par = pour remplacer tout le texte par le tien.\n/annuler pour abandonner.")
	case cbDraftRegen, cbDraftAll:
		if a.Paused() {
			answer("En pause : /resume d'abord.")
			a.reissueDraftButton(ctx, c)
			return true
		}
		d, m, why := a.activeDraftFor(ctx, c)
		if d == nil {
			answer(why)
			return true
		}
		if c.Action == cbDraftAll {
			answer("Destinataires mis à jour.")
			a.startDraft(ctx, draftRequest{m: m, instructions: d.Instructions, literal: d.Body, replyAll: !d.ReplyAll})
		} else {
			answer("Nouvelle version…")
			a.startDraft(ctx, draftRequest{m: m, instructions: d.Instructions, replyAll: d.ReplyAll})
		}
	case cbDraftCancel:
		d, _, why := a.activeDraftFor(ctx, c)
		if d == nil {
			answer(why)
			return true
		}
		if ok, _ := a.st.TransitionDraft(ctx, d.ID, store.DraftActive, store.DraftCancelled, "", a.now()); ok {
			a.retireDraft(ctx, d)
			a.audit(ctx, "telegram", "draft_cancel", fmt.Sprintf("draft=%d", d.ID))
		}
		answer("Brouillon annulé et retiré du dossier Brouillons.")
	case cbDraftSend:
		a.askSendConfirmation(ctx, q, c)
	case cbSendConfirm:
		a.confirmSend(ctx, q, c)
	case cbSendAbort:
		answer("Envoi annulé.")
		if q.Message != nil {
			_ = a.tg.EditMarkup(ctx, a.cfg.Telegram.ChatID, q.Message.MessageID, nil)
		}
	case cbMarkRead:
		m, err := a.st.GetMessage(ctx, c.MessagePK)
		if err != nil {
			answer("Mail introuvable.")
			return true
		}
		if err := a.ops.MarkSeen(ctx, m.Mailbox, m.UIDValidity, m.UID); err != nil {
			a.log.Warn("mark seen", "message_pk", m.ID, "err", err)
			answer("Échec : " + truncateRunes(err.Error(), 150))
			a.refreshFrom(ctx, q, c)
			return true
		}
		a.audit(ctx, "telegram", "mark_seen", fmt.Sprintf("message=%d", m.ID))
		answer("Marqué comme lu.")
	case cbThreadIgnore:
		m, err := a.st.GetMessage(ctx, c.MessagePK)
		if err != nil || m.ThreadID == 0 {
			answer("Fil introuvable.")
			return true
		}
		_ = a.st.AddMute(ctx, threadPattern(m.ThreadID), time.Time{})
		a.audit(ctx, "telegram", "thread_ignore", fmt.Sprintf("thread=%d", m.ThreadID))
		answer("Les prochains mails de ce fil iront au digest.")
	default:
		return false
	}
	return true
}

// refreshFrom rebuilds the keyboard of the notification a button was on.
func (a *App) refreshFrom(ctx context.Context, q *telegram.CallbackQuery, c *store.Callback) {
	if q.Message != nil && c.MessagePK > 0 {
		a.refreshKeyboard(ctx, q.Message.MessageID, c.MessagePK)
	}
}

func threadPattern(id int64) string { return "thread:" + strconv.FormatInt(id, 10) }

// reissueDraftButton keeps a draft button usable after a non-final press.
func (a *App) reissueDraftButton(ctx context.Context, c *store.Callback) {
	d, err := a.st.GetDraft(ctx, c.DraftPK)
	if err != nil || d.TGMessageID == 0 {
		return
	}
	_, complete := a.formatDraftByID(ctx, d)
	_ = a.tg.EditMarkup(ctx, a.cfg.Telegram.ChatID, d.TGMessageID, a.draftKeyboard(ctx, d, complete))
}

func (a *App) formatDraftByID(ctx context.Context, d *store.Draft) (telegram.Text, bool) {
	m, err := a.st.GetMessage(ctx, d.MessagePK)
	if err != nil {
		return telegram.Text{}, false
	}
	return a.formatDraft(m, d, "")
}

// sendGate lists every reason a send must be refused right now.
func (a *App) sendGate(ctx context.Context) string {
	switch {
	case !a.cfg.SMTP.SendEnabled:
		return "Envoi désactivé (smtp.send_enabled: false). Le brouillon est dans ton dossier Brouillons."
	case a.Paused():
		return "En pause : aucun envoi. /resume d'abord."
	}
	n, err := a.st.SentSince(ctx, a.now().Add(-time.Hour))
	if err != nil {
		return "Erreur interne."
	}
	if n >= a.cfg.SMTP.MaxSendsPerHour {
		return fmt.Sprintf("Limite de %d envois par heure atteinte.", a.cfg.SMTP.MaxSendsPerHour)
	}
	return ""
}

// askSendConfirmation is the first click: it shows exactly what will be
// sent to whom, with a single-use confirmation button valid 10 minutes.
func (a *App) askSendConfirmation(ctx context.Context, q *telegram.CallbackQuery, c *store.Callback) {
	answer := func(s string) { _ = a.tg.AnswerCallback(ctx, q.ID, s) }
	d, m, why := a.activeDraftFor(ctx, c)
	if d == nil {
		answer(why)
		return
	}
	if why := a.sendGate(ctx); why != "" {
		answer(truncateRunes(why, 190))
		a.reissueDraftButton(ctx, c)
		return
	}
	if _, complete := a.formatDraft(m, d, ""); !complete {
		answer("Trop long pour être relu ici : envoie-le depuis ta messagerie.")
		return
	}
	answer("")
	var b telegram.Builder
	b.Bold("📤 Confirmer l'envoi ?").Plain("\n")
	b.Plain("À : " + addrLine(d.To) + "\n")
	if len(d.Cc) > 0 {
		b.Plain("Cc : " + addrLine(d.Cc) + "\n")
	}
	b.Plain("Objet : " + Defang(d.Subject) + "\n")
	if w := replyToWarning(m); w != "" {
		b.Plain(w + "\n")
	}
	b.Plain(fmt.Sprintf("Brouillon v%d, tel qu'affiché au-dessus. Ce bouton expire dans 10 minutes.", d.Version))
	yes, _ := a.button(ctx, "✅ Oui, envoyer", cbSendConfirm, d.MessagePK, d.ID, strconv.Itoa(d.Version), sendConfirmTTL)
	no, _ := a.button(ctx, "Annuler", cbSendAbort, d.MessagePK, d.ID, "", sendConfirmTTL)
	if _, err := a.send(ctx, b.Text(), telegram.SendOptions{Markup: &telegram.InlineMarkup{InlineKeyboard: [][]telegram.Button{row(yes, no)}},
		ReplyTo: d.TGMessageID}); err != nil {
		a.log.Warn("send confirmation", "err", err)
	}
	a.reissueDraftButton(ctx, c)
}

// confirmSend is the second click. Every gate is checked again.
func (a *App) confirmSend(ctx context.Context, q *telegram.CallbackQuery, c *store.Callback) {
	answer := func(s string) { _ = a.tg.AnswerCallback(ctx, q.ID, s) }
	d, m, why := a.activeDraftFor(ctx, c)
	if d == nil {
		answer(why)
		return
	}
	if why := a.sendGate(ctx); why != "" {
		answer(truncateRunes(why, 190))
		return
	}
	note, err := a.sendDraft(ctx, d, m, "telegram", func() { answer("Envoi…") })
	switch {
	case errors.Is(err, errRecipients):
		answer("Destinataires incohérents : envoi refusé.")
		return
	case errors.Is(err, errAlreadyHandled):
		answer("Déjà en cours d'envoi.")
		return
	case err != nil:
		a.sendText(ctx, "❌ Échec de l'envoi : "+truncateRunes(err.Error(), 300)+"\nLe brouillon reste dans Brouillons.")
		return
	}
	if q.Message != nil {
		_ = a.tg.EditMarkup(ctx, a.cfg.Telegram.ChatID, q.Message.MessageID, nil)
	}
	if note != "" {
		note = "\n⚠️ " + note
	}
	a.sendText(ctx, "✅ Réponse envoyée à "+addrLine(d.To)+"."+note)
}

var errRecipients = errors.New("destinataires incohérents : envoi refusé")

// sendDraft sends the active draft d of message m. Recipients are
// recomputed from the stored headers and must match what Louis confirmed;
// the draft moves active → sending → sent atomically, so a double click
// sends once (started runs once that transition is won). The returned note reports a failed copy to Sent.
func (a *App) sendDraft(ctx context.Context, d *store.Draft, m *store.Message, actor string, started func()) (string, error) {
	to, cc := reply.Recipients(original(m), a.self, d.ReplyAll)
	if addrLine(toStoreAddrs(to)) != addrLine(d.To) || addrLine(toStoreAddrs(cc)) != addrLine(d.Cc) {
		a.log.Error("recipient mismatch at send time", "draft_id", d.ID)
		return "", errRecipients
	}
	if ok, err := a.st.TransitionDraft(ctx, d.ID, store.DraftActive, store.DraftSending, "", a.now()); err != nil || !ok {
		return "", errAlreadyHandled
	}
	if started != nil {
		started()
	}
	rcpts := reply.Emails(to, cc)
	a.audit(ctx, actor, "send_confirmed", fmt.Sprintf("draft=%d message=%d rcpts=%d", d.ID, d.MessagePK, len(rcpts)))
	if err := a.sendMail(ctx, a.cfg.SMTP, a.fromAddr().Email, rcpts, d.Raw); err != nil {
		_, _ = a.st.TransitionDraft(ctx, d.ID, store.DraftSending, store.DraftActive, truncateRunes(err.Error(), 500), a.now())
		a.audit(ctx, "mailpilot", "send_failed", fmt.Sprintf("draft=%d", d.ID))
		a.log.Warn("smtp send failed", "draft_id", d.ID, "err", err)
		return "", err
	}
	_, _ = a.st.TransitionDraft(ctx, d.ID, store.DraftSending, store.DraftSent, "", a.now())
	a.audit(ctx, "mailpilot", "sent", fmt.Sprintf("draft=%d message=%d", d.ID, d.MessagePK))
	a.log.Info("reply sent", "draft_id", d.ID, "message_pk", d.MessagePK, "rcpts", len(rcpts))
	note := ""
	if a.cfg.IMAP.SaveSent {
		if _, err := a.ops.AppendSent(ctx, d.Raw); err != nil {
			note = "Copie dans Envoyés impossible : " + truncateRunes(err.Error(), 150)
		}
	}
	a.retireDraft(ctx, d)
	return note, nil
}

// takeEdit returns the draft awaiting instructions, if any.
func (a *App) takeEdit() int64 {
	a.drafts.editMu.Lock()
	defer a.drafts.editMu.Unlock()
	id := a.drafts.editDraft
	if id == 0 || a.now().After(a.drafts.editExpiry) {
		a.drafts.editDraft = 0
		return 0
	}
	a.drafts.editDraft = 0
	return id
}

// draftText consumes Louis's instructions after "Modifier".
func (a *App) draftText(ctx context.Context, msg *telegram.Message) bool {
	id := a.takeEdit()
	if id == 0 {
		return false
	}
	d, err := a.st.GetDraft(ctx, id)
	if err != nil || d.Status != store.DraftActive {
		a.sendText(ctx, "Ce brouillon n'est plus actif.")
		return true
	}
	m, err := a.st.GetMessage(ctx, d.MessagePK)
	if err != nil {
		return true
	}
	if a.Paused() {
		a.sendText(ctx, "En pause : /resume d'abord.")
		return true
	}
	text := strings.TrimSpace(msg.Text)
	if lit, ok := strings.CutPrefix(text, "="); ok {
		a.startDraft(ctx, draftRequest{m: m, instructions: d.Instructions, literal: lit, replyAll: d.ReplyAll})
		return true
	}
	instr := strings.TrimSpace(d.Instructions + "\n" + text)
	a.sendText(ctx, "Nouvelle version en cours…")
	a.startDraft(ctx, draftRequest{m: m, instructions: instr, previous: d.Body, replyAll: d.ReplyAll})
	return true
}

func (a *App) cancelEdit(ctx context.Context) {
	if a.takeEdit() != 0 {
		a.sendText(ctx, "Modification abandonnée.")
		return
	}
	a.sendText(ctx, "Rien à annuler.")
}

// maybeAutoDraft drafts automatically when a rule asks for it.
func (a *App) maybeAutoDraft(ctx context.Context, m *store.Message, t *analysis.Triage, eff effects) {
	if !eff.AutoDraft || a.ops == nil || a.Paused() || t == nil || t.Category == "spam_probable" || t.AIInstructions() {
		return
	}
	if _, err := a.st.ActiveDraft(ctx, m.ID); err == nil {
		return
	}
	a.startDraft(ctx, draftRequest{m: m})
}

// recordCalls stores the cost of non-triage LLM calls.
func (a *App) recordCalls(ctx context.Context, task string, pk int64, attempts []analysis.Attempt) {
	for _, at := range attempts {
		call := store.LLMCall{Task: task, Provider: a.provider.Name(), MessagePK: pk, Outcome: at.Status}
		if r := at.Response; r != nil {
			call.Model, call.TokensIn, call.TokensOut = r.Model, r.Usage.InputTokens, r.Usage.OutputTokens
			call.TokensCached, call.CostUSD, call.Latency = r.Usage.CacheReadTokens, r.CostUSD, r.Duration
		}
		if err := a.st.RecordLLMCall(ctx, call); err != nil {
			a.log.Error("record llm call", "err", err)
		}
	}
}
