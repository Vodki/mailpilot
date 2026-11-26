package app

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

// Watchdog thresholds.
const (
	healthStale    = 15 * time.Minute
	digestCatchUp  = 6 * time.Hour // a missed slot is still sent within this window
	maxDigestItems = 150
)

// scheduler drives digests, the IMAP watchdog, the retention purge and the
// pending sweep.
func (a *App) scheduler(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	lastPurge, lastSweep := time.Time{}, a.now()
	for {
		now := a.now()
		a.digestTick(ctx, now)
		a.healthTick(ctx, now)
		if now.Sub(lastPurge) >= 24*time.Hour {
			a.purge(ctx, now)
			lastPurge = now
		}
		if now.Sub(lastSweep) >= 10*time.Minute {
			a.sweep(ctx)
			lastSweep = now
		}
		a.extTick(ctx, now)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *App) digestTick(ctx context.Context, now time.Time) {
	day := a.localDay(now)
	for _, hm := range a.cfg.Digest.Times {
		t, err := time.Parse("15:04", hm)
		if err != nil {
			continue
		}
		at := day.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute)
		if now.Before(at) || now.Sub(at) > digestCatchUp {
			continue
		}
		slot := day.Format("2006-01-02") + " " + hm
		id, ok, err := a.st.ClaimDigest(ctx, slot)
		if err != nil {
			a.log.Error("claim digest", "slot", slot, "err", err)
			continue
		}
		if ok {
			a.sendDigest(ctx, id, "Digest de "+hm, false)
		}
	}
}

// SendDigestNow sends the backlog immediately (/digest, /resume).
func (a *App) SendDigestNow(ctx context.Context, title string) {
	slot := fmt.Sprintf("manual %s #%d", a.now().UTC().Format(time.RFC3339), manualSeq.Add(1)+time.Now().UnixNano())
	id, ok, err := a.st.ClaimDigest(ctx, slot)
	if err != nil || !ok {
		a.log.Error("claim manual digest", "err", err)
		return
	}
	a.sendDigest(ctx, id, title, true)
}

var manualSeq atomic.Int64

var prioOrder = map[string]int{"urgent": 0, "haute": 1, "normale": 2, "basse": 3, "": 4}

func (a *App) sendDigest(ctx context.Context, digestID int64, title string, force bool) {
	items, err := a.st.DigestBacklog(ctx, maxDigestItems+1)
	if err != nil {
		a.log.Error("digest backlog", "err", err)
		return
	}
	if len(items) == 0 {
		if force {
			a.sendText(ctx, title+" : aucun mail en attente.")
		}
		_ = a.st.MarkDigestSent(ctx, digestID)
		return
	}
	more := 0
	if len(items) > maxDigestItems {
		items, more = items[:maxDigestItems], 1 // at least one more; exact count below
		if st, err := a.st.GetStats(ctx, time.Time{}); err == nil {
			more = st.DigestBacklog - maxDigestItems
		}
	}
	prio := func(it store.DigestItem) string {
		if it.Analysis == nil {
			return ""
		}
		return it.Analysis.Priority
	}
	sort.SliceStable(items, func(i, j int) bool {
		return prioOrder[prio(items[i])] < prioOrder[prio(items[j])]
	})

	var lines []telegram.Text
	var head telegram.Builder
	head.Bold(fmt.Sprintf("📬 %s · %d mail%s", title, len(items)+more, plural(len(items)+more)))
	lines = append(lines, head.Text())
	group := "-"
	sections := map[string]string{"urgent": "Urgent", "haute": "Important", "normale": "Normal", "basse": "Faible", "": "Sans analyse"}
	var ids []int64
	for _, it := range items {
		if p := prio(it); p != group {
			group = p
			var g telegram.Builder
			g.Plain("\n").Bold(sections[p])
			lines = append(lines, g.Text())
		}
		lines = append(lines, a.digestLine(it))
		ids = append(ids, it.Message.ID)
	}
	for _, l := range a.digestFooter(ctx) {
		var b telegram.Builder
		b.Plain("\n" + l)
		lines = append(lines, b.Text())
	}
	if more > 0 {
		var b telegram.Builder
		b.Italic(fmt.Sprintf("\n… et %d autre%s au prochain digest.", more, plural(more)))
		lines = append(lines, b.Text())
	}
	for _, chunk := range telegram.Split(lines, telegram.MaxText-100) {
		if _, err := a.send(ctx, chunk, telegram.SendOptions{Silent: true}); err != nil {
			a.log.Warn("digest send failed", "err", err)
			return // backlog kept for the next digest
		}
	}
	if err := a.st.MarkDigested(ctx, digestID, ids); err != nil {
		a.log.Error("mark digested", "err", err)
	}
	_ = a.st.MarkDigestSent(ctx, digestID)
	a.log.Info("digest sent", "digest_id", digestID, "items", len(ids))
}

func (a *App) digestLine(it store.DigestItem) telegram.Text {
	m := it.Message
	var b telegram.Builder
	name := m.From.Name
	if name == "" {
		name = m.From.Email
	}
	b.Plain("• ").Bold(Defang(name)).Plain(" — " + Defang(orDash(m.Subject)))
	if m.HasAttachments {
		b.Plain(" 📎")
	}
	switch {
	case it.Analysis != nil:
		b.Plain("\n   " + Defang(it.Analysis.Summary))
		if it.Analysis.AIInstructions {
			b.Plain(" ⚠️ instructions IA ignorées")
		}
	case m.State == store.StateAnalysisFailed:
		b.Plain("\n   (analyse échouée)")
	}
	return b.Text()
}

func plural(n int) string {
	if n > 1 {
		return "s"
	}
	return ""
}

// healthTick alerts once when IMAP has had no successful cycle for 15
// minutes or authentication fails, and once on recovery.
func (a *App) healthTick(ctx context.Context, now time.Time) {
	if a.watcher == nil {
		return
	}
	h := a.watcher.Health()
	last := h.LastOK
	if last.IsZero() {
		last = a.started
	}
	bad := h.AuthFailed || now.Sub(last) > healthStale
	a.healthMu.Lock()
	alerted := a.healthAlerted
	a.healthAlerted = bad
	a.healthMu.Unlock()
	switch {
	case bad && !alerted:
		msg := fmt.Sprintf("⚠️ IMAP : aucune synchronisation réussie depuis %s.", humanDur(now.Sub(last)))
		if h.AuthFailed {
			msg = "⛔ IMAP : l'authentification échoue. Vérifie le mot de passe (MAILPILOT_IMAP_PASSWORD)."
		}
		if h.LastError != "" {
			msg += "\nDernière erreur : " + truncateRunes(h.LastError, 300)
		}
		a.log.Warn("imap health alert", "auth_failed", h.AuthFailed, "since", last)
		a.sendText(ctx, msg)
	case !bad && alerted:
		a.sendText(ctx, "✅ IMAP : synchronisation rétablie.")
	}
}

func humanDur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d jours", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d h %02d", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d s", int(d.Seconds()))
}

func (a *App) purge(ctx context.Context, now time.Time) {
	n, err := a.st.PurgeBodies(ctx, now.AddDate(0, 0, -a.cfg.Retention.BodyDays))
	if err != nil {
		a.log.Error("retention purge", "err", err)
		return
	}
	c, err := a.st.PurgeCallbacks(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		a.log.Error("callback purge", "err", err)
	}
	if n > 0 || c > 0 {
		a.log.Info("retention purge", "bodies", n, "callbacks", c, "days", a.cfg.Retention.BodyDays)
	}
}

// nextDigest returns the next scheduled digest time ("HH:MM"), for /status.
func (a *App) nextDigest(now time.Time) string {
	local := now.In(a.cfg.Location)
	cur := local.Format("15:04")
	times := append([]string(nil), a.cfg.Digest.Times...)
	sort.Strings(times)
	for _, t := range times {
		if t > cur {
			return t
		}
	}
	if len(times) > 0 {
		return "demain " + times[0]
	}
	return "aucun"
}
