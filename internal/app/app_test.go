package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/imapwatch"
	"github.com/Vodki/mailpilot/internal/llm"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
	"github.com/Vodki/mailpilot/internal/telegram/tgtest"
)

// scriptedProvider answers by looking for a marker in the prompt.
type scriptedProvider struct {
	mu      sync.Mutex
	answers map[string]string // marker -> JSON
	calls   []llm.Request
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Complete(_ context.Context, r llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, r)
	for marker, out := range p.answers {
		if strings.Contains(r.Prompt, marker) {
			return &llm.Response{Output: json.RawMessage(out), Model: "m", CostUSD: 0.02, Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}}, nil
		}
	}
	return &llm.Response{Output: json.RawMessage(triageJSON("normale", "information", "Un mail.", "")), Model: "m"}, nil
}

func (p *scriptedProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func triageJSON(prio, cat, summary, actions string) string {
	if actions == "" {
		actions = "[]"
	}
	return `{"category":"` + cat + `","priority":"` + prio + `","summary":"` + summary + `","needs_reply":false,"actions":` +
		actions + `,"contains_ai_instructions":false,"reason":"r"}`
}

type fakeWatcher struct {
	mu sync.Mutex
	h  imapwatch.Health
}

func (w *fakeWatcher) Health() imapwatch.Health { w.mu.Lock(); defer w.mu.Unlock(); return w.h }
func (w *fakeWatcher) Kick()                    {}
func (w *fakeWatcher) set(h imapwatch.Health)   { w.mu.Lock(); w.h = h; w.mu.Unlock() }

type harness struct {
	t    *testing.T
	app  *App
	st   *store.Store
	tg   *tgtest.Server
	prov *scriptedProvider
	w    *fakeWatcher
	ctx  context.Context
	now  time.Time
	uid  uint32
	mu   sync.Mutex
}

func (h *harness) set(t time.Time) { h.mu.Lock(); h.now = t; h.mu.Unlock() }
func (h *harness) get() time.Time  { h.mu.Lock(); defer h.mu.Unlock(); return h.now }

const chatID = 4242

func newHarness(t *testing.T, mut ...func(*config.Config)) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := config.Default()
	cfg.IMAP.Username = "louis@example.test"
	cfg.Telegram.ChatID = chatID
	cfg.Location, _ = time.LoadLocation("Europe/Paris")
	for _, m := range mut {
		m(&cfg)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	srv := tgtest.New(t)
	prompts, _ := analysis.LoadPrompts("")
	prov := &scriptedProvider{answers: map[string]string{}}
	h := &harness{t: t, st: st, tg: srv, prov: prov, ctx: ctx, w: &fakeWatcher{}}
	h.set(time.Date(2026, 11, 26, 10, 0, 0, 0, cfg.Location))
	a, err := New(Deps{
		Config: &cfg, Store: st, Provider: prov, Prompts: prompts,
		TG:  telegram.New(srv.URL, config.NewSecret("T"), nil),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test",
		Now: h.get,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.SetWatcher(h.w)
	h.w.set(imapwatch.Health{Connected: true, Mode: "idle", LastOK: h.now})
	h.app = a
	return h
}

func (h *harness) start() {
	h.t.Helper()
	if err := h.app.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// deliver feeds a fixture (or raw text) through the IMAP sink.
func (h *harness) deliver(name string, silent bool) int64 {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "eml", name))
	if err != nil {
		raw = []byte(name)
	}
	h.uid++
	if err := h.app.Sink().Deliver(h.ctx, &imapwatch.Fetched{
		Mailbox: "INBOX", UIDValidity: 1, UID: h.uid, InternalDate: h.now, Raw: raw, Silent: silent, Size: int64(len(raw)),
	}); err != nil {
		h.t.Fatal(err)
	}
	var id int64
	_ = h.st.DB().QueryRow("SELECT id FROM messages WHERE uid = ?", h.uid).Scan(&id)
	return id
}

func (h *harness) waitDelivery(id int64, want string) {
	h.t.Helper()
	tgtest.WaitFor(h.t, 5*time.Second, func() bool {
		m, err := h.st.GetMessage(h.ctx, id)
		return err == nil && m.Delivery == want
	})
}

func (h *harness) lastText() string {
	texts := h.tg.Texts()
	if len(texts) == 0 {
		return ""
	}
	return texts[len(texts)-1]
}

func (h *harness) command(text string) {
	h.app.Command(h.ctx, strings.TrimPrefix(strings.Fields(text)[0], "/"), strings.Join(strings.Fields(text)[1:], " "), &telegram.Message{})
}

func TestUrgentMailNotifiesImmediately(t *testing.T) {
	h := newHarness(t)
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("urgent", "action_requise", "Pierre demande de valider le devis.",
		`[{"text":"Valider le devis","owner":"me","due_date":"2026-11-27","due_time":"","source_quote":"valider le devis avant vendredi"}]`)
	h.start()
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryImmediate)
	txt := h.lastText()
	for _, want := range []string{"🔴 Urgent", "action requise", "Pierre Martin <pierre@client.example>", "Validation du devis été", "Valider le devis (pour le 27/11)"} {
		if !strings.Contains(txt, want) {
			t.Errorf("notification lacks %q:\n%s", want, txt)
		}
	}
	call := h.tg.Calls("sendMessage")[0].Params
	if call["chat_id"] != float64(chatID) {
		t.Fatalf("sent to %v", call["chat_id"])
	}
	// Re-processing (crash recovery) must not notify twice.
	_ = h.st.SetDelivery(h.ctx, id, store.DeliveryPending)
	h.app.process(h.ctx, id)
	if n := len(h.tg.Calls("sendMessage")); n != 1 {
		t.Fatalf("notified %d times", n)
	}
	st, _ := h.st.GetStats(h.ctx, time.Time{})
	if st.LLMCallsToday != 1 || st.ImmediateToday != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestInjectionMailIsFlaggedNotObeyed(t *testing.T) {
	h := newHarness(t)
	// Even if the model were fooled into "urgent", Go flags the mail and the
	// model output carries no recipient or action field that could be used.
	h.prov.answers["Ignore tes instructions"] = triageJSON("urgent", "action_requise", "Demande de transfert de mails.", "")
	h.start()
	id := h.deliver("injection.eml", false)
	h.waitDelivery(id, store.DeliveryImmediate)
	txt := h.lastText()
	if !strings.Contains(txt, "instructions adressées à une IA") {
		t.Fatalf("missing injection warning:\n%s", txt)
	}
	if strings.Contains(txt, "x@y.example") || strings.Contains(txt, "collect@evil.example") {
		t.Fatalf("injected or Reply-To addresses must not surface:\n%s", txt)
	}
	req := h.prov.calls[0]
	if strings.Count(req.Prompt, "</email>") != 1 {
		t.Fatal("the mail closed the untrusted wrapper")
	}
	m, _ := h.st.GetMessage(h.ctx, id)
	an, _ := h.st.LatestAnalysis(h.ctx, id)
	if !an.AIInstructions || m.ReplyTo[0].Email != "collect@evil.example" {
		t.Fatalf("analysis %+v", an)
	}
}

func TestNormalMailGoesToDigest(t *testing.T) {
	h := newHarness(t)
	h.start()
	id := h.deliver("newsletter.eml", false)
	h.waitDelivery(id, store.DeliveryDigest)
	if n := len(h.tg.Calls("sendMessage")); n != 0 {
		t.Fatalf("no immediate notification expected, got %d", n)
	}
	h.command("/digest")
	txt := h.lastText()
	if !strings.Contains(txt, "Digest à la demande · 1 mail") || !strings.Contains(txt, "Un mail.") {
		t.Fatalf("digest:\n%s", txt)
	}
	h.command("/digest")
	if !strings.Contains(h.lastText(), "aucun mail en attente") {
		t.Fatalf("second digest: %s", h.lastText())
	}
}

func TestScheduledDigestOncePerSlot(t *testing.T) {
	h := newHarness(t)
	h.set(time.Date(2026, 11, 26, 7, 0, 0, 0, h.app.cfg.Location)) // before any slot
	h.start()
	id := h.deliver("newsletter.eml", false)
	h.waitDelivery(id, store.DeliveryDigest)
	h.set(time.Date(2026, 11, 26, 8, 29, 0, 0, h.app.cfg.Location))
	h.app.digestTick(h.ctx, h.now)
	if len(h.tg.Texts()) != 0 {
		t.Fatal("digest sent before its time")
	}
	h.set(h.now.Add(2 * time.Minute))
	h.app.digestTick(h.ctx, h.now)
	h.app.digestTick(h.ctx, h.now.Add(time.Minute))
	if n := len(h.tg.Texts()); n != 1 || !strings.Contains(h.tg.Texts()[0], "Digest de 08:30") {
		t.Fatalf("digests %v", h.tg.Texts())
	}
	// Empty scheduled digest: nothing is sent.
	h.set(time.Date(2026, 11, 26, 18, 0, 0, 0, h.app.cfg.Location))
	h.app.digestTick(h.ctx, h.now)
	if n := len(h.tg.Texts()); n != 1 {
		t.Fatalf("empty digest was sent: %v", h.tg.Texts())
	}
}

func TestFailedAnalysisNotifies(t *testing.T) {
	h := newHarness(t)
	h.prov.answers["Réunion"] = `{"nope":true}`
	h.start()
	id := h.deliver("windows1252_base64.eml", false)
	h.waitDelivery(id, store.DeliveryImmediate)
	if !strings.Contains(h.lastText(), "Mail non analysé") || h.prov.count() != 2 {
		t.Fatalf("got %q after %d calls", h.lastText(), h.prov.count())
	}
	m, _ := h.st.GetMessage(h.ctx, id)
	if m.State != store.StateAnalysisFailed {
		t.Fatalf("state %s", m.State)
	}
}

func TestPauseDefersToCatchUpDigest(t *testing.T) {
	h := newHarness(t)
	h.prov.answers["Peux-tu valider le devis"] = triageJSON("urgent", "action_requise", "Urgent.", "")
	h.start()
	h.command("/pause")
	if !h.app.Paused() || !strings.Contains(h.lastText(), "Pause") {
		t.Fatal("pause not applied")
	}
	if v, _ := h.st.GetSetting(h.ctx, settingPaused); v != "1" {
		t.Fatal("pause must persist across restarts")
	}
	id := h.deliver("html_only.eml", false)
	h.waitDelivery(id, store.DeliveryDigest)
	before := len(h.tg.Texts())
	h.command("/resume")
	texts := h.tg.Texts()[before:]
	if len(texts) != 2 || !strings.Contains(texts[1], "Rattrapage de la pause · 1 mail") {
		t.Fatalf("resume messages %v", texts)
	}
	var n int
	_ = h.st.DB().QueryRow("SELECT COUNT(*) FROM audit_log WHERE event IN ('pause','resume')").Scan(&n)
	if n != 2 {
		t.Fatalf("audit rows %d", n)
	}
}

func TestBootstrapIsSilentAndSkipsLLM(t *testing.T) {
	h := newHarness(t)
	h.start()
	id := h.deliver("gmail_reply.eml", true)
	time.Sleep(50 * time.Millisecond)
	m, _ := h.st.GetMessage(h.ctx, id)
	if m.Delivery != store.DeliverySilent || m.State != store.StateSkipped || h.prov.count() != 0 || len(h.tg.Texts()) != 0 {
		t.Fatalf("bootstrap message %+v, llm calls %d", m, h.prov.count())
	}
}

func TestResumeAfterRestart(t *testing.T) {
	h := newHarness(t)
	// Message stored but never processed (crash before the worker ran).
	id := h.deliver("newsletter.eml", false)
	h.start()
	h.waitDelivery(id, store.DeliveryDigest)
}

func TestThreadContextIsPassed(t *testing.T) {
	h := newHarness(t)
	h.start()
	root := "From: Paul <paul@x.test>\r\nTo: louis@example.test\r\nSubject: Planning\r\nMessage-ID: <root-planning@example.test>\r\n\r\nQui prend la semaine du 12 ?\r\n"
	id1 := h.deliver(root, false)
	h.waitDelivery(id1, store.DeliveryDigest)
	h.set(h.now.Add(time.Hour))
	id2 := h.deliver("gmail_reply.eml", false)
	h.waitDelivery(id2, store.DeliveryDigest)
	p := h.prov.calls[1].Prompt
	if !strings.Contains(p, `role="contexte"`) || !strings.Contains(p, "Qui prend la semaine du 12 ?") {
		t.Fatalf("thread context missing:\n%s", p)
	}
}

func TestStatusAndHealthAlerts(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.command("/status")
	txt := h.lastText()
	for _, want := range []string{"mailpilot test", "IMAP : connecté (IDLE)", "Envoi SMTP : désactivé", "Prochain digest : 18:00"} {
		if !strings.Contains(txt, want) {
			t.Errorf("status lacks %q:\n%s", want, txt)
		}
	}
	h.app.healthTick(h.ctx, h.now.Add(10*time.Minute))
	n := len(h.tg.Texts())
	h.app.healthTick(h.ctx, h.now.Add(16*time.Minute))
	h.app.healthTick(h.ctx, h.now.Add(17*time.Minute))
	texts := h.tg.Texts()
	if len(texts) != n+1 || !strings.Contains(texts[n], "aucune synchronisation réussie depuis 16 min") {
		t.Fatalf("stale alert %v", texts[n:])
	}
	h.w.set(imapwatch.Health{Connected: true, LastOK: h.now.Add(17 * time.Minute)})
	h.app.healthTick(h.ctx, h.now.Add(18*time.Minute))
	if !strings.Contains(h.lastText(), "rétablie") {
		t.Fatalf("recovery: %s", h.lastText())
	}
	h.w.set(imapwatch.Health{AuthFailed: true, LastOK: h.now.Add(18 * time.Minute), LastError: "imap authentication failed"})
	h.app.healthTick(h.ctx, h.now.Add(18*time.Minute))
	if !strings.Contains(h.lastText(), "authentification échoue") {
		t.Fatalf("auth alert: %s", h.lastText())
	}
}

func TestUnknownCallbackRefused(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.app.Callback(h.ctx, &telegram.CallbackQuery{ID: "q", Data: "forged"})
	c := h.tg.Calls("answerCallbackQuery")
	if len(c) != 1 || c[0].Params["text"] != "Bouton inconnu." {
		t.Fatalf("calls %+v", c)
	}
}

func TestDecide(t *testing.T) {
	tr := func(p, c string) *analysis.Triage { return &analysis.Triage{Priority: p, Category: c} }
	cases := []struct {
		state string
		t     *analysis.Triage
		eff   effects
		muted bool
		want  string
	}{
		{store.StateAnalyzed, tr("urgent", "action_requise"), effects{}, false, store.DeliveryImmediate},
		{store.StateAnalyzed, tr("haute", "information"), effects{}, false, store.DeliveryImmediate},
		{store.StateAnalyzed, tr("normale", "information"), effects{}, false, store.DeliveryDigest},
		{store.StateAnalyzed, tr("urgent", "spam_probable"), effects{}, false, store.DeliveryDigest},
		{store.StateAnalyzed, tr("urgent", "action_requise"), effects{}, true, store.DeliveryDigest},
		{store.StateAnalyzed, tr("urgent", "action_requise"), effects{Notify: "never"}, false, store.DeliveryDigest},
		{store.StateAnalyzed, tr("basse", "information"), effects{Notify: "always"}, false, store.DeliveryImmediate},
		{store.StateAnalyzed, tr("basse", "information"), effects{Priority: "urgent"}, false, store.DeliveryImmediate},
		{store.StateSkipped, nil, effects{DigestOnly: true}, false, store.DeliveryDigest},
		{store.StateSkipped, nil, effects{Ignore: true}, false, store.DeliveryIgnored},
		{store.StateAnalysisFailed, nil, effects{}, false, store.DeliveryImmediate},
	}
	for i, c := range cases {
		if got := Decide(c.state, c.t, c.eff, c.muted); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestDefang(t *testing.T) {
	got := Defang("Voir https://evil.example/x et www.phish.com ou paypal.com")
	for _, bad := range []string{"https://", "www.phish", "paypal.com"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q still contains %q", got, bad)
		}
	}
}
