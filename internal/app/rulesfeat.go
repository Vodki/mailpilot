package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/rules"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

const maxProfileBytes = 32 << 10

// Reload re-reads rules.yaml, profile.md and the prompt overrides. On any
// error the previous configuration stays active and the error is returned.
func (a *App) Reload(_ context.Context) error {
	rs, err := rules.Load(a.cfg.RulesPath)
	if err != nil {
		return fmt.Errorf("rules.yaml : %w", err)
	}
	profile, err := os.ReadFile(a.cfg.ProfilePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		profile = nil
	case err != nil:
		return fmt.Errorf("profile.md : %w", err)
	case len(profile) > maxProfileBytes:
		return fmt.Errorf("profile.md : %d octets, maximum %d", len(profile), maxProfileBytes)
	}
	prompts, err := analysis.LoadPrompts(a.cfg.PromptsDir)
	if err != nil {
		return fmt.Errorf("prompts : %w", err)
	}
	an, err := analysis.New(a.provider, prompts, a.cfg.Location)
	if err != nil {
		return fmt.Errorf("prompts : %w", err)
	}
	dr, err := analysis.NewDrafter(a.provider, prompts, a.cfg.Location)
	if err != nil {
		return fmt.Errorf("prompts : %w", err)
	}
	p := string(profile)
	a.drafter.Store(dr)
	a.rules.Store(rs)
	a.profileText.Store(&p)
	a.prompts.Store(&prompts)
	a.analyzer.Store(an)
	a.log.Info("configuration reloaded", "rules", len(rs.Rules), "profile_bytes", len(profile))
	return nil
}

// ReloadAndReport reloads and tells Louis the outcome (SIGHUP, /reload).
func (a *App) ReloadAndReport(ctx context.Context) {
	if err := a.Reload(ctx); err != nil {
		a.log.Warn("reload failed, keeping previous configuration", "err", err)
		a.sendText(ctx, "⚠️ Rechargement refusé, l'ancienne configuration reste active.\n"+truncateRunes(err.Error(), 1500))
		return
	}
	n := 0
	if rs := a.rules.Load(); rs != nil {
		n = len(rs.Rules)
	}
	a.sendText(ctx, fmt.Sprintf("🔄 Configuration rechargée : %d règle%s, profil %s.", n, plural(n), a.profileState()))
	a.audit(ctx, "telegram", "reload", "")
}

func (a *App) profileState() string {
	if strings.TrimSpace(a.profile()) == "" {
		return "vide"
	}
	return "chargé"
}

func (a *App) profile() string {
	if p := a.profileText.Load(); p != nil {
		return *p
	}
	return ""
}

func (a *App) factsOf(m *store.Message) rules.Facts {
	addressed := "none"
	if m.IsDirect {
		addressed = "direct"
	} else {
		for _, c := range m.Cc {
			if a.self[strings.ToLower(c.Email)] {
				addressed = "cc"
			}
		}
	}
	return rules.Facts{From: strings.ToLower(m.From.Email), Subject: m.Subject, HasAttachment: m.HasAttachments,
		Addressed: addressed, ListID: m.ListID}
}

func (a *App) evalRules(ctx context.Context, m *store.Message) effects {
	e := a.rules.Load().Eval(a.factsOf(m))
	if len(e.Rules) > 0 && m.State == store.StateNew {
		if err := a.st.RecordRuleHits(ctx, m.ID, e.Rules); err != nil {
			a.log.Error("rule hits", "err", err)
		}
	}
	return effects{
		Ignore: e.Ignore, SkipLLM: e.SkipLLM, DigestOnly: e.DigestOnly, Priority: e.Priority, Notify: e.Notify,
		Extra: e.Extra, Todoist: e.Todoist, AutoDraft: e.AutoDraft, Rules: e.Rules,
	}
}

// mutePattern normalises "/mute" input to an address or a domain.
func mutePattern(s string) (string, bool) {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), "<>"))
	s = strings.TrimPrefix(s, "@")
	if s == "" || strings.ContainsAny(s, " \t/") || !strings.Contains(s, ".") {
		return "", false
	}
	return s, true
}

func muteMatches(pattern, from string) bool {
	from = strings.ToLower(from)
	if strings.Contains(pattern, "@") {
		return pattern == from
	}
	_, dom, _ := strings.Cut(from, "@")
	return dom == pattern || strings.HasSuffix(dom, "."+pattern)
}

func (a *App) isMuted(ctx context.Context, m *store.Message) bool {
	mutes, err := a.st.Mutes(ctx, a.now())
	if err != nil {
		a.log.Error("mutes", "err", err)
		return false
	}
	thread := threadPattern(m.ThreadID)
	for _, mu := range mutes {
		if (m.ThreadID > 0 && mu.Pattern == thread) || muteMatches(mu.Pattern, m.From.Email) {
			return true
		}
	}
	return false
}

// cmdMute: /mute [adresse|domaine] [jours]. Without an argument, as a reply
// to a notification, mutes that mail's sender.
func (a *App) cmdMute(ctx context.Context, args string, msg *telegram.Message) {
	fields := strings.Fields(args)
	if len(fields) == 0 && msg != nil && msg.ReplyTo != nil {
		if m, err := a.st.MessageByNotification(ctx, msg.ReplyTo.MessageID); err == nil {
			fields = []string{m.From.Email}
		}
	}
	if len(fields) == 0 {
		a.listMutes(ctx)
		return
	}
	pat, ok := mutePattern(fields[0])
	if !ok {
		a.sendText(ctx, "Usage : /mute adresse@domaine.fr ou /mute domaine.fr [jours]. Ou réponds /mute à une notification.")
		return
	}
	var until time.Time
	if len(fields) > 1 {
		days, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(fields[1], "j"), "d"))
		if err != nil || days < 1 || days > 3650 {
			a.sendText(ctx, "Durée invalide : donne un nombre de jours, par exemple /mute "+pat+" 7")
			return
		}
		until = a.now().AddDate(0, 0, days)
	}
	if err := a.st.AddMute(ctx, pat, until); err != nil {
		a.log.Error("add mute", "err", err)
		a.sendText(ctx, "Erreur en enregistrant la sourdine.")
		return
	}
	a.audit(ctx, "telegram", "mute", pat)
	msgTxt := "🔕 " + pat + " : ses mails iront seulement au digest"
	if !until.IsZero() {
		msgTxt += ", jusqu'au " + until.In(a.cfg.Location).Format("02/01/2006")
	}
	a.sendText(ctx, msgTxt+". /unmute "+pat+" pour annuler.")
}

func (a *App) listMutes(ctx context.Context) {
	mutes, err := a.st.Mutes(ctx, a.now())
	if err != nil || len(mutes) == 0 {
		a.sendText(ctx, "Aucune sourdine. Usage : /mute adresse@domaine.fr ou /mute domaine.fr [jours].")
		return
	}
	var b strings.Builder
	b.WriteString("Sourdines actives :\n")
	for _, m := range mutes {
		if strings.HasPrefix(m.Pattern, "thread:") {
			continue
		}
		b.WriteString("• " + m.Pattern)
		if !m.Until.IsZero() {
			b.WriteString(" (jusqu'au " + m.Until.In(a.cfg.Location).Format("02/01") + ")")
		}
		b.WriteString("\n")
	}
	a.sendText(ctx, strings.TrimSpace(b.String()))
}

func (a *App) cmdUnmute(ctx context.Context, args string) {
	pat, ok := mutePattern(args)
	if !ok {
		a.sendText(ctx, "Usage : /unmute adresse@domaine.fr ou /unmute domaine.fr")
		return
	}
	removed, err := a.st.RemoveMute(ctx, pat)
	switch {
	case err != nil:
		a.sendText(ctx, "Erreur en retirant la sourdine.")
	case !removed:
		a.sendText(ctx, "Pas de sourdine sur "+pat+".")
	default:
		a.audit(ctx, "telegram", "unmute", pat)
		a.sendText(ctx, "🔔 "+pat+" : notifications rétablies.")
	}
}
