package app

import (
	"context"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

// Feature hooks: the pipeline calls these at fixed points; each feature
// (rules, Todoist, drafts, search) plugs in here.

func (a *App) startExtensions(ctx context.Context) error {
	return a.Reload(ctx)
}

// extTick runs periodic work.
func (a *App) extTick(context.Context, time.Time) {}

// afterInsert runs once per newly stored message.
func (a *App) afterInsert(context.Context, int64, *store.Message) {}

// afterAnalysis runs before delivery.
func (a *App) afterAnalysis(ctx context.Context, m *store.Message, t *analysis.Triage, eff effects) {
	a.handleActions(ctx, m, t, eff)
	a.maybeAutoDraft(ctx, m, t, eff)
}

// notifyLines adds feature lines to an immediate notification.
func (a *App) notifyLines(ctx context.Context, m *store.Message) []string {
	return a.actionLines(ctx, m)
}

// messageKeyboard returns the inline buttons of a notification.
func (a *App) messageKeyboard(ctx context.Context, m *store.Message, _ *analysis.Triage) *telegram.InlineMarkup {
	var rows [][]telegram.Button
	rows = append(rows, a.messageButtons(ctx, m)...)
	rows = append(rows, a.actionButtons(ctx, m)...)
	if len(rows) == 0 {
		return nil
	}
	return &telegram.InlineMarkup{InlineKeyboard: rows}
}

// digestFooter adds lines at the end of a digest.
func (a *App) digestFooter(ctx context.Context) []string {
	if n := a.openReminderCount(ctx); n > 0 {
		return []string{"➕ " + itoa(int64(n)) + " rappel" + plural(n) + " à confirmer : /rappels"}
	}
	return nil
}

// extCommands adds commands to the menu.
func (a *App) extCommands() []telegram.BotCommand {
	return []telegram.BotCommand{
		{Command: "mute", Description: "Envoyer un expéditeur au digest seulement"},
		{Command: "unmute", Description: "Annuler une sourdine"},
		{Command: "rappels", Description: "Rappels Todoist à confirmer"},
		{Command: "reload", Description: "Recharger règles, profil et prompts"},
		{Command: "annuler", Description: "Abandonner la modification d'un brouillon"},
	}
}

// extCommand handles feature commands; false if unknown.
func (a *App) extCommand(ctx context.Context, name, args string, m *telegram.Message) bool {
	switch name {
	case "mute":
		a.cmdMute(ctx, args, m)
	case "unmute":
		a.cmdUnmute(ctx, args)
	case "rappels", "reminders":
		a.cmdReminders(ctx)
	case "reload":
		a.ReloadAndReport(ctx)
	case "annuler", "cancel":
		a.cancelEdit(ctx)
	default:
		return false
	}
	return true
}

// extStatus adds lines to /status.
func (a *App) extStatus(ctx context.Context) []string {
	n := 0
	if rs := a.rules.Load(); rs != nil {
		n = len(rs.Rules)
	}
	todo := "désactivé"
	if a.todo != nil {
		todo = "activé"
		if c := a.openReminderCount(ctx); c > 0 {
			todo += ", " + itoa(int64(c)) + " rappel" + plural(c) + " à confirmer"
		}
	}
	mutes, _ := a.st.Mutes(ctx, a.now())
	return []string{
		"Règles : " + itoa(int64(n)) + " · profil " + a.profileState() + " · sourdines : " + itoa(int64(len(mutes))),
		"Todoist : " + todo,
	}
}

// extText handles free text; false if not consumed.
func (a *App) extText(ctx context.Context, m *telegram.Message) bool { return a.draftText(ctx, m) }

// extCallback handles a consumed callback token; false if unknown.
func (a *App) extCallback(ctx context.Context, q *telegram.CallbackQuery, c *store.Callback) bool {
	return a.todoCallback(ctx, q, c) || a.draftCallback(ctx, q, c)
}
