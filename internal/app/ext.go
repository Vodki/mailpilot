package app

import (
	"context"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
)

// Hooks for later-phase features (rules, Todoist, drafts, search). Phase 1
// has none.

func (a *App) startExtensions(context.Context) error { return nil }

// extTick runs periodic work of later phases.
func (a *App) extTick(context.Context, time.Time) {}

// evalRules applies rules.yaml (phase 2).
func (a *App) evalRules(context.Context, *store.Message) effects { return effects{} }

// isMuted reports a /mute on the sender or thread (phase 2).
func (a *App) isMuted(context.Context, *store.Message) bool { return false }

// profile returns profile.md (phase 2).
func (a *App) profile() string { return "" }

// afterInsert runs once per newly stored message (search index, phase 4).
func (a *App) afterInsert(context.Context, int64, *store.Message) {}

// afterAnalysis runs before delivery (Todoist, auto drafts: phases 2-3).
func (a *App) afterAnalysis(context.Context, *store.Message, *analysis.Triage, effects) {}

// messageKeyboard returns the inline buttons of a notification (phase 3).
func (a *App) messageKeyboard(context.Context, *store.Message, *analysis.Triage) *telegram.InlineMarkup {
	return nil
}

// extCommands adds later-phase commands to the menu.
func (a *App) extCommands() []telegram.BotCommand { return nil }

// extCommand handles later-phase commands; false if unknown.
func (a *App) extCommand(context.Context, string, string, *telegram.Message) bool { return false }

// extStatus adds lines to /status.
func (a *App) extStatus(context.Context) []string { return nil }

// extText handles free text; false if not consumed.
func (a *App) extText(context.Context, *telegram.Message) bool { return false }

// extCallback handles a consumed callback token; false if unknown.
func (a *App) extCallback(context.Context, *telegram.CallbackQuery, *store.Callback) bool {
	return false
}
