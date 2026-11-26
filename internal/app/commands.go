package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

// Commands is the bot menu.
func (a *App) Commands() []telegram.BotCommand {
	cmds := []telegram.BotCommand{
		{Command: "status", Description: "État du service"},
		{Command: "digest", Description: "Envoyer le digest maintenant"},
		{Command: "pause", Description: "Suspendre les actions sortantes"},
		{Command: "resume", Description: "Reprendre (avec digest de rattrapage)"},
	}
	return append(cmds, a.extCommands()...)
}

// Command implements telegram.Handler.
func (a *App) Command(ctx context.Context, name, args string, m *telegram.Message) {
	a.log.Info("telegram command", "command", name)
	switch name {
	case "start", "help", "aide":
		a.sendText(ctx, a.helpText())
	case "status":
		a.cmdStatus(ctx)
	case "digest":
		a.SendDigestNow(ctx, "Digest à la demande")
	case "pause":
		a.setPaused(ctx, true)
		a.sendText(ctx, "⏸ Pause : plus de notification immédiate, de rappel Todoist, de brouillon automatique ni d'envoi. La lecture et l'analyse continuent. /resume pour reprendre.")
	case "resume":
		was := a.Paused()
		a.setPaused(ctx, false)
		a.sendText(ctx, "▶️ Reprise des actions sortantes.")
		if was {
			a.SendDigestNow(ctx, "Rattrapage de la pause")
		}
	default:
		if !a.extCommand(ctx, name, args, m) {
			a.sendText(ctx, "Commande inconnue. /help pour la liste.")
		}
	}
}

func (a *App) setPaused(ctx context.Context, p bool) {
	a.paused.Store(p)
	v := "0"
	if p {
		v = "1"
	}
	if err := a.st.SetSetting(ctx, settingPaused, v); err != nil {
		a.log.Error("persist pause", "err", err)
	}
	ev := "resume"
	if p {
		ev = "pause"
	}
	a.audit(ctx, "telegram", ev, "")
}

func (a *App) helpText() string {
	var b strings.Builder
	b.WriteString("mailpilot surveille ta boîte et te prévient ici.\n\n")
	for _, c := range a.Commands() {
		fmt.Fprintf(&b, "/%s : %s\n", c.Command, c.Description)
	}
	return strings.TrimSpace(b.String())
}

func (a *App) cmdStatus(ctx context.Context) {
	now := a.now()
	st, err := a.st.GetStats(ctx, a.localDay(now))
	if err != nil {
		a.log.Error("stats", "err", err)
	}
	var b telegram.Builder
	b.Bold("mailpilot " + a.version).Plain(" · actif depuis " + humanDur(now.Sub(a.started)) + "\n")
	if a.watcher != nil {
		h := a.watcher.Health()
		switch {
		case h.Connected && !h.LastOK.IsZero():
			mode := "IDLE"
			if h.Mode == "poll" {
				mode = "relève toutes les " + a.cfg.IMAP.PollInterval.String()
			}
			b.Plain(fmt.Sprintf("IMAP : connecté (%s), dernière synchro il y a %s\n", mode, humanDur(now.Sub(h.LastOK))))
		case h.LastError != "":
			b.Plain("IMAP : déconnecté (" + truncateRunes(h.LastError, 200) + ")\n")
		default:
			b.Plain("IMAP : connexion en cours\n")
		}
	}
	pause, send := "non", "désactivé"
	if a.Paused() {
		pause = "OUI"
	}
	if a.cfg.SMTP.SendEnabled {
		send = "activé (double validation)"
	}
	b.Plain("Pause : " + pause + " · Envoi SMTP : " + send + "\n")
	b.Plain(fmt.Sprintf("Aujourd'hui : %d mail%s, %d notification%s, %d en attente du digest",
		st.MessagesToday, plural(st.MessagesToday), st.ImmediateToday, plural(st.ImmediateToday), st.DigestBacklog))
	if st.AnalysisFailed > 0 {
		b.Plain(fmt.Sprintf(", %d analyse%s échouée%s", st.AnalysisFailed, plural(st.AnalysisFailed), plural(st.AnalysisFailed)))
	}
	b.Plain("\n")
	b.Plain(fmt.Sprintf("LLM : %d appel%s, coût estimé %s $\n", st.LLMCallsToday, plural(st.LLMCallsToday),
		strings.Replace(strconv.FormatFloat(st.LLMCostTodayUSD, 'f', 2, 64), ".", ",", 1)))
	b.Plain("Prochain digest : " + a.nextDigest(now) + "\n")
	for _, l := range a.extStatus(ctx) {
		b.Plain(l + "\n")
	}
	if _, err := a.send(ctx, b.Text(), telegram.SendOptions{}); err != nil {
		a.log.Warn("status send", "err", err)
	}
}

// Text implements telegram.Handler (free text, e.g. a draft edit).
func (a *App) Text(ctx context.Context, m *telegram.Message) {
	if a.extText(ctx, m) {
		return
	}
	a.sendText(ctx, "Je ne comprends que les commandes. /ask ta question pour chercher dans tes mails, /help pour la liste.")
}

// Callback implements telegram.Handler. Tokens are single-use and expire;
// the action is looked up server-side, never parsed from the button.
func (a *App) Callback(ctx context.Context, q *telegram.CallbackQuery) {
	c, err := a.st.ConsumeCallback(ctx, q.Data, a.now())
	switch {
	case errors.Is(err, store.ErrCallbackUsed):
		_ = a.tg.AnswerCallback(ctx, q.ID, "Bouton déjà utilisé.")
		return
	case errors.Is(err, store.ErrCallbackExpired):
		_ = a.tg.AnswerCallback(ctx, q.ID, "Bouton expiré.")
		return
	case err != nil:
		_ = a.tg.AnswerCallback(ctx, q.ID, "Bouton inconnu.")
		return
	}
	a.log.Info("telegram callback", "action", c.Action, "message_pk", c.MessagePK, "draft_pk", c.DraftPK)
	if !a.extCallback(ctx, q, c) {
		_ = a.tg.AnswerCallback(ctx, q.ID, "Action inconnue.")
	}
}
