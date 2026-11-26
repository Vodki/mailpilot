package app

import (
	"context"
	"regexp"
	"strings"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

// Decide returns how a message reaches Louis. Pure function: the policy is
// Go code, the LLM only provides a priority suggestion.
func Decide(state string, t *analysis.Triage, eff effects, muted bool) string {
	if eff.Ignore {
		return store.DeliveryIgnored
	}
	if eff.Notify == "never" {
		return store.DeliveryDigest
	}
	if eff.Notify == "always" {
		return store.DeliveryImmediate
	}
	if eff.DigestOnly || muted {
		return store.DeliveryDigest
	}
	switch state {
	case store.StateAnalysisFailed:
		return store.DeliveryImmediate // "mail non analysé": Louis must know
	case store.StateSkipped:
		if eff.Priority == "urgent" || eff.Priority == "haute" {
			return store.DeliveryImmediate
		}
		return store.DeliveryDigest
	}
	if t == nil {
		return store.DeliveryDigest
	}
	prio := effectivePriority(t, eff)
	if t.Category == "spam_probable" {
		return store.DeliveryDigest
	}
	if prio == "urgent" || prio == "haute" {
		return store.DeliveryImmediate
	}
	return store.DeliveryDigest
}

func effectivePriority(t *analysis.Triage, eff effects) string {
	if eff.Priority != "" {
		return eff.Priority
	}
	if t == nil {
		return ""
	}
	return t.Priority
}

const (
	kindImmediate = "immediate"
	kindDigest    = "digest"
)

// deliver routes the message and sends the immediate notification. The
// notification is claimed before sending so a crash cannot double-notify;
// delivery stays "pending" until the outcome is known.
func (a *App) deliver(ctx context.Context, m *store.Message, t *analysis.Triage, eff effects) {
	d := Decide(m.State, t, eff, a.isMuted(ctx, m))
	if d == store.DeliveryImmediate && a.Paused() {
		d = store.DeliveryDigest // caught up by the /resume digest
	}
	if d != store.DeliveryImmediate {
		if err := a.st.SetDelivery(ctx, m.ID, d); err != nil {
			a.log.Error("set delivery", "message_pk", m.ID, "err", err)
		}
		return
	}
	ok, err := a.st.ClaimNotification(ctx, m.ID, kindImmediate)
	if err != nil {
		a.log.Error("claim notification", "message_pk", m.ID, "err", err)
		return
	}
	if ok {
		text := a.formatImmediate(m, t, eff, a.notifyLines(ctx, m))
		sent, err := a.send(ctx, text, telegram.SendOptions{Markup: a.messageKeyboard(ctx, m, t)})
		if err != nil {
			_ = a.st.ReleaseNotification(ctx, m.ID, kindImmediate)
			a.log.Warn("immediate notification failed; falling back to digest", "message_pk", m.ID, "err", err)
			_ = a.st.SetDelivery(ctx, m.ID, store.DeliveryDigest)
			return
		}
		_ = a.st.SetNotificationMessage(ctx, m.ID, kindImmediate, sent.MessageID)
		a.log.Info("notified", "message_pk", m.ID, "tg_message_id", sent.MessageID)
	}
	if err := a.st.SetDelivery(ctx, m.ID, store.DeliveryImmediate); err != nil {
		a.log.Error("set delivery", "message_pk", m.ID, "err", err)
	}
}

var (
	urlRe    = regexp.MustCompile(`(?i)\b(https?|ftp)://`)
	wwwRe    = regexp.MustCompile(`(?i)\bwww\.`)
	domainRe = regexp.MustCompile(`(?i)\b([a-z0-9-]+)\.(com|fr|net|org|io|co|info|biz|ru|xyz|top|app|link|ly|me)\b`)
)

// Defang breaks URLs in text that came from a mail or from the model, so
// Telegram does not turn them into clickable links. Addresses in headers
// are rendered separately and keep their form.
func Defang(s string) string {
	s = urlRe.ReplaceAllStringFunc(s, func(m string) string {
		return strings.Replace(strings.Replace(m, "http", "hxxp", 1), "://", "[:]//", 1)
	})
	s = wwwRe.ReplaceAllString(s, "www[.]")
	return domainRe.ReplaceAllString(s, "$1[.]$2")
}

var prioLabel = map[string]string{"urgent": "🔴 Urgent", "haute": "🟠 Important", "normale": "🔵 Normal", "basse": "⚪ Faible"}

var catLabel = map[string]string{
	"action_requise": "action requise", "reponse_attendue": "réponse attendue", "rendez_vous": "rendez-vous",
	"facture_paiement": "facture / paiement", "information": "information", "newsletter": "newsletter",
	"notification_auto": "notification automatique", "personnel": "personnel", "spam_probable": "spam probable", "autre": "autre",
}

// headerAddr renders an address for display. The address is defanged too:
// a display name can carry a URL.
func headerAddr(x store.Address) string {
	if x.Name == "" || strings.EqualFold(x.Name, x.Email) {
		return x.Email
	}
	return Defang(x.Name) + " <" + x.Email + ">"
}

func (a *App) formatImmediate(m *store.Message, t *analysis.Triage, eff effects, extra []string) telegram.Text {
	var b telegram.Builder
	switch {
	case m.State == store.StateAnalysisFailed:
		b.Bold("❓ Mail non analysé").Plain("\n")
	case t == nil:
		label := prioLabel[eff.Priority]
		if label == "" {
			label = "📩 Nouveau mail"
		}
		b.Bold(label).Plain("\n")
	default:
		label := prioLabel[effectivePriority(t, eff)]
		b.Bold(label)
		if c := catLabel[t.Category]; c != "" {
			b.Plain(" · " + c)
		}
		b.Plain("\n")
	}
	b.Plain("De : ").Plain(headerAddr(m.From)).Plain("\n")
	b.Plain("Objet : ").Plain(Defang(orDash(m.Subject))).Plain("\n")
	b.Plain("Reçu : " + m.ReceivedAt.In(a.cfg.Location).Format("02/01 15:04"))
	if !m.IsDirect {
		b.Plain(" (en copie)")
	}
	if m.HasAttachments {
		b.Plain(" 📎")
	}
	b.Plain("\n")
	if t != nil {
		b.Plain("\n").Plain(Defang(t.Summary)).Plain("\n")
		var mine []analysis.Action
		for _, act := range t.Actions {
			if act.Owner == "me" {
				mine = append(mine, act)
			}
		}
		if len(mine) > 0 {
			b.Plain("\n").Bold("À faire").Plain("\n")
			for _, act := range mine {
				b.Plain("• " + Defang(act.Text))
				if due := a.formatDue(act); due != "" {
					b.Plain(" (" + due + ")")
				}
				b.Plain("\n")
			}
		}
		if t.AIInstructions() {
			b.Plain("\n⚠️ Ce mail contient des instructions adressées à une IA. Elles ont été ignorées.\n")
		}
	} else if m.State == store.StateAnalysisFailed {
		b.Plain("\nL'analyse automatique a échoué. Ouvre le mail dans ta messagerie.\n")
	}
	if len(extra) > 0 {
		b.Plain("\n")
		for _, l := range extra {
			b.Plain(l + "\n")
		}
	}
	if len(eff.Rules) > 0 {
		b.Italic("Règle : " + strings.Join(eff.Rules, ", ")).Plain("\n")
	}
	return b.Text()
}

func (a *App) formatDue(act analysis.Action) string {
	if act.DueDate == "" {
		return ""
	}
	s := act.DueDate
	if len(s) == 10 {
		s = s[8:10] + "/" + s[5:7]
	}
	if act.DueTime != "" {
		s += " " + act.DueTime
	}
	return "pour le " + s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(sans objet)"
	}
	return s
}
