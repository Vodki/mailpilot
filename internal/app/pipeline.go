package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/llm"
	"github.com/Vodki/mailpilot/internal/store"
)

// maxAnalysisAttempts bounds provider retries (timeouts, crashes) across
// restarts; an invalid output is retried once inside Analyze.
const maxAnalysisAttempts = 3

// effects are the decisions of the rules engine for one message.
type effects struct {
	Ignore     bool
	SkipLLM    bool
	DigestOnly bool
	Priority   string // forces the priority when set
	Notify     string // always | never | auto ("" = auto)
	Extra      string // extra_instructions for the LLM
	Todoist    string // auto | confirm | off ("" = config default)
	AutoDraft  bool
	Rules      []string
}

// process advances one message through its state machine. It is idempotent
// and safe to call again after a crash at any point.
func (a *App) process(ctx context.Context, id int64) {
	m, err := a.st.GetMessage(ctx, id)
	if err != nil {
		a.log.Error("load message", "message_pk", id, "err", err)
		return
	}
	eff := a.evalRules(ctx, m)

	if m.State == store.StateNew {
		switch {
		case eff.Ignore:
			_ = a.st.SetState(ctx, id, store.StateSkipped, "")
			_ = a.st.SetDelivery(ctx, id, store.DeliveryIgnored)
			a.log.Info("message ignored by rule", "message_pk", id, "rules", eff.Rules)
			return
		case eff.SkipLLM:
			m.State = store.StateSkipped
			_ = a.st.SetState(ctx, id, m.State, "")
		default:
			state, retry := a.analyze(ctx, m, eff)
			if retry {
				return
			}
			m.State = state
		}
	}
	if m.Delivery != store.DeliveryPending {
		return
	}
	tri := a.latestTriage(ctx, id)
	a.afterAnalysis(ctx, m, tri, eff)
	a.deliver(ctx, m, tri, eff)
}

// analyze runs the LLM triage and records every attempt. retry=true means a
// provider failure was rescheduled and the message stays "new".
func (a *App) analyze(ctx context.Context, m *store.Message, eff effects) (state string, retry bool) {
	n, err := a.st.IncAttempts(ctx, m.ID)
	if err != nil {
		a.log.Error("attempts", "message_pk", m.ID, "err", err)
		return store.StateNew, true
	}
	in, err := a.analysisInput(ctx, m, eff)
	if err != nil {
		a.log.Error("analysis input", "message_pk", m.ID, "err", err)
		return store.StateNew, true
	}
	out, aerr := a.analyzer.Analyze(ctx, in)
	a.recordAttempts(ctx, m.ID, n, out)

	switch {
	case aerr != nil && ctx.Err() != nil:
		return store.StateNew, true
	case errors.Is(aerr, llm.ErrToolsExposed):
		// Fatal configuration problem: never retry, tell Louis.
		a.log.Error("llm provider exposes tools; analysis refused", "message_pk", m.ID)
		a.sendText(ctx, "⛔ Le fournisseur LLM expose des outils au modèle : analyse refusée. Vérifie l'installation de claude.")
		_ = a.st.SetState(ctx, m.ID, store.StateAnalysisFailed, "tools exposed")
		return store.StateAnalysisFailed, false
	case aerr != nil:
		if n < maxAnalysisAttempts {
			delay := time.Duration(n*n) * time.Minute
			a.log.Warn("analysis provider error, will retry", "message_pk", m.ID, "attempt", n, "retry_in", delay.String())
			time.AfterFunc(delay, func() { a.Enqueue(m.ID) })
			return store.StateNew, true
		}
		_ = a.st.SetState(ctx, m.ID, store.StateAnalysisFailed, "provider error")
		a.log.Warn("analysis failed", "message_pk", m.ID, "attempts", n)
		return store.StateAnalysisFailed, false
	case out.Triage == nil:
		_ = a.st.SetState(ctx, m.ID, store.StateAnalysisFailed, "invalid output")
		a.log.Warn("analysis output invalid twice", "message_pk", m.ID)
		return store.StateAnalysisFailed, false
	}
	_ = a.st.SetState(ctx, m.ID, store.StateAnalyzed, "")
	a.log.Info("message analyzed", "message_pk", m.ID, "category", out.Triage.Category, "priority", out.Triage.Priority,
		"actions", len(out.Triage.Actions), "ai_instructions", out.Triage.AIInstructions())
	return store.StateAnalyzed, false
}

func (a *App) recordAttempts(ctx context.Context, pk int64, n int, out *analysis.Outcome) {
	if out == nil {
		return
	}
	for _, at := range out.Attempts {
		row := &store.Analysis{
			MessagePK: pk, Attempt: (n-1)*2 + at.N, Provider: a.provider.Name(), Model: out.Model,
			PromptHash: out.PromptHash, Status: at.Status, Error: truncateRunes(at.Error, 2000),
		}
		if at.Status == "ok" && out.Triage != nil {
			b, _ := json.Marshal(out.Triage)
			row.ResultJSON = string(b)
			row.Category, row.Priority, row.Summary = out.Triage.Category, out.Triage.Priority, out.Triage.Summary
			row.AIInstructions = out.Triage.AIInstructions()
		}
		if err := a.st.SaveAnalysis(ctx, row); err != nil {
			a.log.Error("save analysis", "message_pk", pk, "err", err)
		}
		call := store.LLMCall{Task: "triage", Provider: a.provider.Name(), MessagePK: pk, Outcome: at.Status}
		if r := at.Response; r != nil {
			call.Model, call.TokensIn, call.TokensOut = r.Model, r.Usage.InputTokens, r.Usage.OutputTokens
			call.TokensCached, call.CostUSD, call.Latency = r.Usage.CacheReadTokens, r.CostUSD, r.Duration
		}
		if err := a.st.RecordLLMCall(ctx, call); err != nil {
			a.log.Error("record llm call", "err", err)
		}
	}
}

// latestTriage decodes the last successful analysis, or nil.
func (a *App) latestTriage(ctx context.Context, pk int64) *analysis.Triage {
	an, err := a.st.LatestAnalysis(ctx, pk)
	if err != nil {
		return nil
	}
	var t analysis.Triage
	if json.Unmarshal([]byte(an.ResultJSON), &t) != nil {
		return nil
	}
	return &t
}

// analysisInput converts stored data into the analyzer input.
func (a *App) analysisInput(ctx context.Context, m *store.Message, eff effects) (analysis.Input, error) {
	in := analysis.Input{Email: emailOf(m), Profile: a.profile(), Extra: eff.Extra, Now: a.now()}
	if m.ThreadID > 0 {
		prev, err := a.st.ThreadMessages(ctx, m.ThreadID, 6)
		if err != nil {
			return in, err
		}
		for _, p := range prev {
			if p.ID != m.ID && !p.ReceivedAt.After(m.ReceivedAt) {
				in.Thread = append(in.Thread, emailOf(p))
			}
		}
	}
	return in, nil
}

func emailOf(m *store.Message) analysis.Email {
	e := analysis.Email{
		From: addrString(m.From), Subject: m.Subject, Date: m.SentAt, Direct: m.IsDirect,
		ListID: m.ListID, Text: m.TextClean,
	}
	if e.Date.IsZero() {
		e.Date = m.ReceivedAt
	}
	for _, x := range m.To {
		e.To = append(e.To, addrString(x))
	}
	for _, x := range m.Cc {
		e.Cc = append(e.Cc, addrString(x))
	}
	for _, at := range m.Attachments {
		if !at.Inline {
			e.Attachments = append(e.Attachments, fmt.Sprintf("%s (%s, %s)", at.Filename, at.MIMEType, humanSize(at.Size)))
		}
	}
	return e
}

func addrString(x store.Address) string {
	if x.Name == "" {
		return x.Email
	}
	return x.Name + " <" + x.Email + ">"
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f Mo", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d ko", n>>10)
	}
	return fmt.Sprintf("%d o", n)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
