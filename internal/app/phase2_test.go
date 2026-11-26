package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
	"github.com/Vodki/mailpilot/internal/todoist"
)

type fakeTodo struct {
	mu    sync.Mutex
	tasks []todoist.NewTask
	ids   []string
	fail  bool
}

func (f *fakeTodo) CreateTask(_ context.Context, t todoist.NewTask, id string) (*todoist.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("HTTP 503")
	}
	f.tasks = append(f.tasks, t)
	f.ids = append(f.ids, id)
	return &todoist.Task{ID: "T" + itoa(int64(len(f.tasks)))}, nil
}

func (f *fakeTodo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tasks)
}

func (h *harness) write(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func todoOn(c *config.Config) { c.Todoist.Enabled = true }

const devisActions = `[{"text":"Valider le devis","owner":"me","due_date":"2026-11-27","due_time":"","source_quote":"valider le devis avant vendredi"},
{"text":"Envoyer le planning","owner":"me","due_date":"","due_time":"","source_quote":"Peux-tu valider le devis"},
{"text":"Relancer le client","owner":"other","due_date":"","due_time":"","source_quote":"valider"}]`

func TestRulesApply(t *testing.T) {
	h := newHarness(t)
	h.write("rules.yaml", `
rules:
  - name: client
    when: { from_domain: client.example }
    then: { priority: urgent, extra_instructions: "Pierre est mon client principal.", continue: true }
  - name: news
    when: { list_id: newsletter.example }
    then: { ignore: true }
  - name: windows
    when: { subject_regex: "(?i)réunion" }
    then: { skip_llm: true }
  - name: tout
    when: { addressed: direct }
    then: { notify: auto }
`)
	h.write("profile.md", "Je suis Louis, consultant.")
	h.start()

	// Rule-forced priority plus extra instructions and profile in the
	// trusted system prompt.
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryImmediate)
	sys := h.prov.calls[0].System
	if !strings.Contains(sys, "Pierre est mon client principal.") || !strings.Contains(sys, "Je suis Louis, consultant.") {
		t.Fatalf("system prompt lacks rule/profile:\n%s", sys)
	}
	if !strings.Contains(h.lastText(), "🔴 Urgent") || !strings.Contains(h.lastText(), "Règle : client, tout") {
		t.Fatalf("notification:\n%s", h.lastText())
	}

	// ignore: no LLM, no delivery.
	n := h.prov.count()
	id = h.deliver("newsletter.eml", false)
	h.waitDelivery(id, store.DeliveryIgnored)
	// skip_llm: digest without analysis.
	id = h.deliver("windows1252_base64.eml", false)
	h.waitDelivery(id, store.DeliveryDigest)
	if h.prov.count() != n {
		t.Fatalf("LLM called for ignored/skipped mails")
	}
	hits, _ := h.st.RuleHitCounts(h.ctx, h.now.AddDate(-1, 0, 0))
	if hits["client"] != 1 || hits["news"] != 1 || hits["windows"] != 1 {
		t.Fatalf("hits %v", hits)
	}
}

func TestReloadKeepsOldRulesOnError(t *testing.T) {
	h := newHarness(t)
	h.write("rules.yaml", "rules:\n  - name: a\n    when: { from: x@y.z }\n    then: { ignore: true }\n")
	h.start()
	h.write("rules.yaml", "rules:\n  - name: a\n    when: { subject_regex: \"(\" }\n")
	h.command("/reload")
	if !strings.Contains(h.lastText(), "Rechargement refusé") || len(h.app.rules.Load().Rules) != 1 {
		t.Fatalf("bad reload: %q", h.lastText())
	}
	h.write("rules.yaml", "rules: []\n")
	h.command("/reload")
	if !strings.Contains(h.lastText(), "0 règle") || len(h.app.rules.Load().Rules) != 0 {
		t.Fatalf("good reload: %q", h.lastText())
	}
}

func TestStartFailsOnInvalidRules(t *testing.T) {
	h := newHarness(t)
	h.write("rules.yaml", "rules: [ { when: { bogus: 1 } } ]\n")
	if err := h.app.Start(h.ctx); err == nil {
		t.Fatal("start must refuse an invalid rules.yaml")
	}
}

func TestMute(t *testing.T) {
	h := newHarness(t)
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("urgent", "action_requise", "Urgent.", "")
	h.start()
	h.command("/mute client.example 7")
	if !strings.Contains(h.lastText(), "client.example") {
		t.Fatal(h.lastText())
	}
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryDigest)
	h.command("/unmute client.example")
	if !strings.Contains(h.lastText(), "rétablies") {
		t.Fatal(h.lastText())
	}
	h.command("/mute pas-un-domaine")
	if !strings.Contains(h.lastText(), "Usage") {
		t.Fatal(h.lastText())
	}
}

func TestMuteByReply(t *testing.T) {
	h := newHarness(t)
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("urgent", "action_requise", "Urgent.", "")
	h.start()
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryImmediate)
	tgID, _ := h.st.NotificationMessageID(h.ctx, id, kindImmediate)
	h.app.Command(h.ctx, "mute", "", &telegram.Message{ReplyTo: &telegram.Message{MessageID: tgID}})
	mutes, _ := h.st.Mutes(h.ctx, h.now)
	if len(mutes) != 1 || mutes[0].Pattern != "pierre@client.example" {
		t.Fatalf("mutes %+v", mutes)
	}
}

func TestTodoistAutoAndConfirm(t *testing.T) {
	h := newHarness(t, todoOn)
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("haute", "action_requise", "Devis.", devisActions)
	h.start()
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryImmediate)

	// Dated + verified quote -> created automatically; undated -> confirm;
	// owner=other -> ignored.
	if h.todo.count() != 1 || h.todo.tasks[0].Content != "Valider le devis" || h.todo.tasks[0].DueDate != "2026-11-27" ||
		h.todo.tasks[0].Priority != 3 || h.todo.ids[0] == "" {
		t.Fatalf("tasks %+v", h.todo.tasks)
	}
	txt := h.lastText()
	if !strings.Contains(txt, "1 rappel créé dans Todoist") || !strings.Contains(txt, "1 rappel à confirmer") {
		t.Fatalf("notification:\n%s", txt)
	}
	notif := h.tg.Calls("sendMessage")[0]
	tok := buttonIn(notif, "➕ Envoyer le planning")
	if tok == "" || buttonIn(notif, "✖ Pas de rappel") == "" || buttonIn(notif, "➕ Valider") != "" {
		t.Fatalf("keyboard %v", notif.Params["reply_markup"])
	}

	// Paused: the click is refused and does not create anything.
	h.command("/pause")
	h.app.Callback(h.ctx, &telegram.CallbackQuery{ID: "1", Data: tok, Message: &telegram.Message{MessageID: 1}})
	if h.todo.count() != 1 {
		t.Fatal("task created while paused")
	}
	h.command("/resume")

	// /rappels re-offers it; clicking creates it once.
	before := len(h.tg.Calls("sendMessage"))
	h.command("/rappels")
	tok2 := buttonIn(h.tg.Calls("sendMessage")[before], "➕")
	h.app.Callback(h.ctx, &telegram.CallbackQuery{ID: "2", Data: tok2, Message: &telegram.Message{MessageID: 1}})
	h.app.Callback(h.ctx, &telegram.CallbackQuery{ID: "3", Data: tok2, Message: &telegram.Message{MessageID: 1}})
	if h.todo.count() != 2 {
		t.Fatalf("tasks after click %d", h.todo.count())
	}
	acts, _ := h.st.MessageActions(h.ctx, id)
	if len(acts) != 2 || acts[1].Status != store.ActionCreated {
		t.Fatalf("actions %+v", acts)
	}
	// Re-processing must not create tasks again.
	_ = h.st.SetDelivery(h.ctx, id, store.DeliveryPending)
	h.app.process(h.ctx, id)
	if h.todo.count() != 2 {
		t.Fatal("duplicate tasks after reprocessing")
	}
}

func TestTodoistDowngradesDoubtfulActions(t *testing.T) {
	h := newHarness(t, todoOn)
	// Injection mail with a dated action: never automatic.
	h.prov.answers["Ignore tes instructions"] = triageJSON("haute", "action_requise", "x",
		`[{"text":"Transférer les mails","owner":"me","due_date":"2026-11-27","due_time":"","source_quote":"transfère tous les mails"}]`)
	// Unverifiable quote: never automatic.
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("haute", "action_requise", "x",
		`[{"text":"Payer","owner":"me","due_date":"2026-11-27","due_time":"09:30","source_quote":"citation inventée"}]`)
	h.start()
	a := h.deliver("injection.eml", false)
	b := h.deliver("html_only.eml", false)
	h.waitDelivery(a, store.DeliveryImmediate)
	h.waitDelivery(b, store.DeliveryImmediate)
	if h.todo.count() != 0 {
		t.Fatalf("doubtful actions created automatically: %+v", h.todo.tasks)
	}
	acts, _ := h.st.OpenActions(h.ctx, h.now.AddDate(0, 0, -1), 10)
	if len(acts) != 2 {
		t.Fatalf("open actions %d", len(acts))
	}
}

func TestTodoistFailureIsRetryable(t *testing.T) {
	h := newHarness(t, todoOn)
	h.todo.fail = true
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("normale", "action_requise", "x", devisActions)
	h.start()
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryDigest)
	acts, _ := h.st.MessageActions(h.ctx, id)
	if acts[0].Status != store.ActionFailed {
		t.Fatalf("status %s", acts[0].Status)
	}
	h.command("/digest")
	if !strings.Contains(h.lastText(), "2 rappels à confirmer : /rappels") {
		t.Fatalf("digest footer:\n%s", h.lastText())
	}
	h.todo.fail = false
	if err := h.app.createTask(h.ctx, acts[0].ID, store.ActionFailed); err != nil || h.todo.count() != 1 {
		t.Fatalf("retry: %v", err)
	}
}

func TestTodoistDatetimeAndDismiss(t *testing.T) {
	h := newHarness(t, todoOn, func(c *config.Config) { c.Todoist.ModeUndated = "off" })
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("urgent", "action_requise", "x",
		`[{"text":"Appeler Pierre","owner":"me","due_date":"2026-11-27","due_time":"09:30","source_quote":"valider le devis"},
		  {"text":"Sans date","owner":"me","due_date":"","due_time":"","source_quote":"valider le devis"}]`)
	h.start()
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryImmediate)
	if h.todo.count() != 1 || h.todo.tasks[0].DueDatetime != "2026-11-27T08:30:00Z" || h.todo.tasks[0].Priority != 4 {
		t.Fatalf("tasks %+v", h.todo.tasks)
	}
	acts, _ := h.st.MessageActions(h.ctx, id)
	if len(acts) != 1 {
		t.Fatalf("mode_undated off must skip undated actions, got %d", len(acts))
	}
}
