package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

const askSearchLimit = 8

// cmdAsk: /ask question. Full-text search first, then the LLM answers from
// the hits only; every citation is checked against the hits it was given.
// One question at a time.
func (a *App) cmdAsk(ctx context.Context, args string) {
	q := strings.TrimSpace(args)
	if q == "" {
		a.sendText(ctx, "Usage : /ask ta question. Par exemple : /ask quand passe le couvreur ?")
		return
	}
	if !a.asking.CompareAndSwap(false, true) {
		a.sendText(ctx, "Une question est déjà en cours, attends sa réponse.")
		return
	}
	a.goRun(func() {
		defer a.asking.Store(false)
		if err := a.answer(ctx, q); err != nil {
			a.log.Warn("ask failed", "err", err)
			a.sendText(ctx, "⚠️ Recherche impossible : "+truncateRunes(err.Error(), 500))
		}
	})
}

func (a *App) answer(ctx context.Context, q string) error {
	hits, err := a.st.Search(ctx, q, askSearchLimit)
	if err != nil {
		return fmt.Errorf("recherche : %w", err)
	}
	var msgs []*store.Message
	var sources []analysis.AskSource
	for _, h := range hits {
		m, err := a.st.GetMessage(ctx, h.MessagePK)
		if err != nil {
			continue
		}
		msgs = append(msgs, m)
		sources = append(sources, analysis.AskSource{Email: emailOf(m), Summary: h.Summary})
	}
	a.log.Info("ask", "terms", len(store.SearchTerms(q)), "hits", len(msgs))
	if len(msgs) == 0 {
		a.sendText(ctx, "🔎 Aucun mail indexé ne correspond. Essaie d'autres mots (expéditeur, objet, mot-clé).")
		return nil
	}
	asker := a.asker.Load()
	if asker == nil {
		return fmt.Errorf("assistant non configuré")
	}
	r, attempts, err := asker.Ask(ctx, analysis.AskInput{Question: q, Sources: sources, Now: a.now()})
	a.recordCalls(ctx, "ask", 0, attempts)
	if err != nil {
		return err
	}
	_, err = a.send(ctx, a.formatAnswer(r, msgs), telegram.SendOptions{})
	return err
}

// formatAnswer renders the answer and the cited mails. Citations point only
// at messages the search returned (checked by the asker).
func (a *App) formatAnswer(r *analysis.AskResult, msgs []*store.Message) telegram.Text {
	var b telegram.Builder
	b.Bold("🔎 Réponse").Plain("\n\n")
	b.Plain(truncateRunes(Defang(r.Answer), 2000))
	if len(r.CitedRefs) > 0 {
		b.Plain("\n\n").Bold("Sources").Plain("\n")
		for _, n := range r.CitedRefs {
			m := msgs[n-1]
			when := m.SentAt
			if when.IsZero() {
				when = m.ReceivedAt
			}
			b.Plain(fmt.Sprintf("[%d] %s · ", n, when.In(a.cfg.Location).Format("02/01/2006")))
			b.Plain(headerAddr(m.From)).Plain(" · ").Plain(truncateRunes(Defang(orDash(m.Subject)), 120)).Plain("\n")
		}
	}
	return b.Text()
}
