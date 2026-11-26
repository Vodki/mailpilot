package app

import (
	"strings"
	"testing"
	"time"

	"github.com/Vodki/mailpilot/internal/llm"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram/tgtest"
)

func (h *harness) askCalls() []llm.Request {
	h.prov.mu.Lock()
	defer h.prov.mu.Unlock()
	var out []llm.Request
	for _, c := range h.prov.calls {
		if c.Task == "ask" {
			out = append(out, c)
		}
	}
	return out
}

func (h *harness) waitText(sub string) string {
	h.t.Helper()
	var got string
	tgtest.WaitFor(h.t, 5*time.Second, func() bool {
		for _, s := range h.tg.Texts() {
			if strings.Contains(s, sub) {
				got = s
				return true
			}
		}
		return false
	})
	return got
}

func TestAskAnswersWithCheckedCitations(t *testing.T) {
	h := newHarness(t)
	h.start()
	id1 := h.deliver("html_only.eml", true)
	id2 := h.deliver("injection.eml", true)
	if id1 == 0 || id2 == 0 {
		t.Fatal("fixtures not stored")
	}
	// First answer cites a source that does not exist: rejected, retried.
	h.prov.askAnswers = []string{
		`{"answer":"Pierre attend ta validation.","cited_refs":[7]}`,
		`{"answer":"Pierre attend ta validation du devis, voir https://evil.example/x.","cited_refs":[1]}`,
	}
	h.command("/ask où en est le devis de Pierre ?")
	txt := h.waitText("🔎 Réponse")
	if !strings.Contains(txt, "Pierre attend ta validation du devis") || !strings.Contains(txt, "[1] ") ||
		!strings.Contains(txt, "Validation du devis été") {
		t.Fatalf("answer or source missing:\n%s", txt)
	}
	if strings.Contains(txt, "https://evil.example") {
		t.Fatalf("links in the answer must be defanged:\n%s", txt)
	}
	calls := h.askCalls()
	if len(calls) != 2 {
		t.Fatalf("ask calls %d", len(calls))
	}
	p := calls[0].Prompt
	if !strings.Contains(p, `<email untrusted="true" role="extrait">`) || !strings.Contains(p, "où en est le devis de Pierre") {
		t.Fatalf("prompt not built from wrapped hits:\n%s", p)
	}
	if calls[0].Schema == nil || !strings.Contains(string(calls[0].Schema), "cited_refs") {
		t.Fatal("ask must use the ask schema")
	}
	var n int
	_ = h.st.DB().QueryRow("SELECT count(*) FROM llm_calls WHERE task = 'ask'").Scan(&n)
	if n != 2 {
		t.Fatalf("llm_calls for ask: %d", n)
	}
}

func TestAskWithoutHitsSkipsLLM(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.deliver("html_only.eml", true)
	h.command("/ask zzqxw introuvable")
	h.waitText("Aucun mail indexé")
	h.command("/ask")
	h.waitText("Usage : /ask")
	if n := len(h.askCalls()); n != 0 {
		t.Fatalf("LLM called %d times without hits", n)
	}
}

func TestAskInStatusAndHelp(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.deliver("html_only.eml", true)
	h.command("/status")
	if txt := h.waitText("Recherche :"); !strings.Contains(txt, "1 mail indexé") {
		t.Fatalf("status:\n%s", txt)
	}
	h.command("/help")
	h.waitText("/ask : Poser une question")
	var st string
	_ = h.st.DB().QueryRow("SELECT state FROM messages LIMIT 1").Scan(&st)
	if st != store.StateSkipped {
		t.Fatalf("silent import state %q", st)
	}
}
