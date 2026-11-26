package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
	"github.com/Vodki/mailpilot/internal/todoist"
)

// TaskCreator is the Todoist surface (an interface for tests).
type TaskCreator interface {
	CreateTask(ctx context.Context, t todoist.NewTask, requestID string) (*todoist.Task, error)
}

// Callback actions for reminders.
const (
	cbTodoAdd     = "todo_add"
	cbTodoDismiss = "todo_dismiss"
	callbackTTL   = 48 * time.Hour
)

// todoistMode decides, in Go, whether an action becomes a task on its own.
// Anything doubtful is downgraded to "confirm": unverified quote, dropped
// date, a mail that talks to an AI, or the service being paused.
func (a *App) todoistMode(act analysis.Action, t *analysis.Triage, eff effects) string {
	mode := a.cfg.Todoist.ModeUndated
	if act.DueDate != "" {
		mode = a.cfg.Todoist.ModeDated
	}
	if eff.Todoist != "" {
		mode = eff.Todoist
	}
	if mode == "auto" && (!act.QuoteVerified || act.DateDropped || t.AIInstructions() || a.Paused()) {
		mode = "confirm"
	}
	return mode
}

// handleActions records Louis's actions and creates the automatic ones.
func (a *App) handleActions(ctx context.Context, m *store.Message, t *analysis.Triage, eff effects) {
	if a.todo == nil || t == nil {
		return
	}
	for i, act := range t.Actions {
		if act.Owner != "me" || strings.TrimSpace(act.Text) == "" {
			continue
		}
		mode := a.todoistMode(act, t, eff)
		if mode == "off" {
			continue
		}
		row := &store.Action{MessagePK: m.ID, Idx: i, Text: act.Text, DueDate: act.DueDate, DueTime: act.DueTime,
			QuoteVerified: act.QuoteVerified, Priority: effectivePriority(t, eff), Status: store.ActionProposed}
		id, inserted, err := a.st.InsertAction(ctx, row)
		if err != nil {
			a.log.Error("insert action", "message_pk", m.ID, "err", err)
			continue
		}
		if inserted && mode == "auto" {
			_ = a.createTask(ctx, id, store.ActionProposed) // failure is recorded on the action
		}
	}
}

// createTask claims an action and creates its Todoist task.
func (a *App) createTask(ctx context.Context, actionID int64, from ...string) error {
	ok, err := a.st.ClaimAction(ctx, actionID, from...)
	if err != nil {
		return err
	}
	if !ok {
		return errAlreadyHandled
	}
	act, err := a.st.GetAction(ctx, actionID)
	if err != nil {
		return err
	}
	m, err := a.st.GetMessage(ctx, act.MessagePK)
	if err != nil {
		return err
	}
	task := todoist.NewTask{
		Content:     Defang(truncateRunes(act.Text, 300)),
		Description: a.taskDescription(m),
		ProjectID:   a.cfg.Todoist.ProjectID,
		Labels:      a.cfg.Todoist.Labels,
		Priority:    map[string]int{"urgent": 4, "haute": 3}[act.Priority],
	}
	if act.DueDate != "" {
		if act.DueTime != "" {
			if d, err := time.ParseInLocation("2006-01-02 15:04", act.DueDate+" "+act.DueTime, a.cfg.Location); err == nil {
				task.DueDatetime = d.UTC().Format(time.RFC3339)
			}
		}
		if task.DueDatetime == "" {
			task.DueDate = act.DueDate
		}
	}
	if task.Priority == 0 {
		task.Priority = 1
	}
	res, err := a.todo.CreateTask(ctx, task, "mailpilot-action-"+strconv.FormatInt(act.ID, 10))
	if err != nil {
		_ = a.st.SetActionStatus(ctx, act.ID, store.ActionFailed, "", truncateRunes(err.Error(), 500))
		a.log.Warn("todoist task failed", "action_id", act.ID, "err", err)
		return err
	}
	_ = a.st.SetActionStatus(ctx, act.ID, store.ActionCreated, res.ID, "")
	a.audit(ctx, "mailpilot", "todoist_task", fmt.Sprintf("action=%d task=%s", act.ID, res.ID))
	a.log.Info("todoist task created", "action_id", act.ID, "message_pk", act.MessagePK)
	return nil
}

var (
	errAlreadyHandled = errors.New("déjà traité")
	errPaused         = errors.New("en pause")
)

func (a *App) taskDescription(m *store.Message) string {
	from := m.From.Email
	if m.From.Name != "" {
		from = m.From.Name + " (" + m.From.Email + ")"
	}
	return Defang(fmt.Sprintf("Mail de %s, objet « %s », reçu le %s. Créé par mailpilot.",
		from, truncateRunes(orDash(m.Subject), 150), m.ReceivedAt.In(a.cfg.Location).Format("02/01/2006 15:04")))
}

// actionLines describes the reminders of a message in its notification.
func (a *App) actionLines(ctx context.Context, m *store.Message) []string {
	if a.todo == nil {
		return nil
	}
	acts, _ := a.st.MessageActions(ctx, m.ID)
	var created, proposed int
	for _, act := range acts {
		switch act.Status {
		case store.ActionCreated:
			created++
		case store.ActionProposed, store.ActionFailed:
			proposed++
		}
	}
	var out []string
	if created > 0 {
		out = append(out, fmt.Sprintf("✅ %d rappel%s créé%s dans Todoist", created, plural(created), plural(created)))
	}
	if proposed > 0 {
		out = append(out, fmt.Sprintf("➕ %d rappel%s à confirmer", proposed, plural(proposed)))
	}
	return out
}

// actionButtons returns one row per open reminder, plus a dismiss row.
func (a *App) actionButtons(ctx context.Context, m *store.Message) [][]telegram.Button {
	if a.todo == nil {
		return nil
	}
	acts, _ := a.st.MessageActions(ctx, m.ID)
	var rows [][]telegram.Button
	for _, act := range acts {
		if act.Status != store.ActionProposed && act.Status != store.ActionFailed {
			continue
		}
		tok, err := a.st.CreateCallback(ctx, cbTodoAdd, m.ID, 0, strconv.FormatInt(act.ID, 10), a.now().Add(callbackTTL))
		if err != nil {
			continue
		}
		label := "➕ " + truncateRunes(act.Text, 40)
		if act.DueDate != "" {
			label += " · " + act.DueDate[8:10] + "/" + act.DueDate[5:7]
		}
		rows = append(rows, []telegram.Button{{Text: label, CallbackData: tok}})
	}
	if len(rows) > 0 {
		if tok, err := a.st.CreateCallback(ctx, cbTodoDismiss, m.ID, 0, "", a.now().Add(callbackTTL)); err == nil {
			rows = append(rows, []telegram.Button{{Text: "✖ Pas de rappel", CallbackData: tok}})
		}
	}
	return rows
}

// todoCallback handles the reminder buttons.
func (a *App) todoCallback(ctx context.Context, q *telegram.CallbackQuery, c *store.Callback) bool {
	switch c.Action {
	case cbTodoAdd:
		id, _ := strconv.ParseInt(c.Arg, 10, 64)
		var err error
		if a.Paused() {
			err = errPaused
		} else {
			err = a.createTask(ctx, id, store.ActionProposed, store.ActionFailed)
		}
		switch {
		case errors.Is(err, errPaused):
			_ = a.tg.AnswerCallback(ctx, q.ID, "En pause : /resume d'abord.")
		case err == nil:
			_ = a.tg.AnswerCallback(ctx, q.ID, "Rappel créé dans Todoist.")
		case errors.Is(err, errAlreadyHandled):
			_ = a.tg.AnswerCallback(ctx, q.ID, "Déjà traité.")
		default:
			_ = a.tg.AnswerCallback(ctx, q.ID, "Échec Todoist, réessaie plus tard.")
		}
	case cbTodoDismiss:
		acts, _ := a.st.MessageActions(ctx, c.MessagePK)
		for _, act := range acts {
			if act.Status == store.ActionProposed || act.Status == store.ActionFailed {
				_ = a.st.SetActionStatus(ctx, act.ID, store.ActionDismissed, "", "")
			}
		}
		_ = a.tg.AnswerCallback(ctx, q.ID, "Rappels ignorés.")
	default:
		return false
	}
	if q.Message != nil {
		a.refreshKeyboard(ctx, q.Message.MessageID, c.MessagePK)
	}
	return true
}

// refreshKeyboard rebuilds the buttons of a notification after an action.
func (a *App) refreshKeyboard(ctx context.Context, tgMessageID, messagePK int64) {
	m, err := a.st.GetMessage(ctx, messagePK)
	if err != nil {
		return
	}
	kb := a.messageKeyboard(ctx, m, a.latestTriage(ctx, messagePK))
	if err := a.tg.EditMarkup(ctx, a.cfg.Telegram.ChatID, tgMessageID, kb); err != nil {
		a.log.Warn("edit keyboard", "err", err)
	}
}

// cmdReminders lists open reminders (/rappels), one message per mail.
func (a *App) cmdReminders(ctx context.Context) {
	if a.todo == nil {
		a.sendText(ctx, "Todoist n'est pas activé (todoist.enabled et MAILPILOT_TODOIST_TOKEN).")
		return
	}
	acts, err := a.st.OpenActions(ctx, a.now().AddDate(0, 0, -14), 50)
	if err != nil || len(acts) == 0 {
		a.sendText(ctx, "Aucun rappel en attente.")
		return
	}
	seen := map[int64]bool{}
	shown := 0
	for _, act := range acts {
		if seen[act.MessagePK] || shown >= 10 {
			continue
		}
		seen[act.MessagePK] = true
		shown++
		m, err := a.st.GetMessage(ctx, act.MessagePK)
		if err != nil {
			continue
		}
		var b telegram.Builder
		b.Bold(Defang(orDash(m.Subject))).Plain("\nDe : " + headerAddr(m.From))
		rows := a.actionButtons(ctx, m)
		if _, err := a.send(ctx, b.Text(), telegram.SendOptions{Markup: &telegram.InlineMarkup{InlineKeyboard: rows}, Silent: true}); err != nil {
			a.log.Warn("reminders send", "err", err)
			return
		}
	}
}

// openReminderCount is shown in digests.
func (a *App) openReminderCount(ctx context.Context) int {
	if a.todo == nil {
		return 0
	}
	acts, err := a.st.OpenActions(ctx, a.now().AddDate(0, 0, -14), 100)
	if err != nil {
		return 0
	}
	return len(acts)
}
