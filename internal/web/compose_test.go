package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Vodki/mailpilot/internal/store"
)

func (f *fixture) postJSON(path string, fields url.Values) (int, actionResult) {
	f.t.Helper()
	fields.Set("csrf", f.csrf)
	rec := f.do("POST", path, fields, jsonHdr)
	var res actionResult
	_ = jsonUnmarshal(rec.Body.Bytes(), &res)
	return rec.Code, res
}

func TestReplyDraftAndDoubleConfirmedSend(t *testing.T) {
	f := newFixture(t)
	f.login()
	roof := itoa(f.ids["roof"])
	pane := f.do("GET", "/v/inbox?m="+roof, nil, map[string]string{"X-Partial": "pane"}).Body.String()
	if !strings.Contains(pane, "Répondre avec l'IA") || !strings.Contains(pane, "Écrire moi-même") {
		t.Fatalf("no reply buttons:\n%s", pane)
	}
	if code, res := f.postJSON("/brouillon", url.Values{"id": {roof}, "op": {"nouveau"}}); code != http.StatusOK || !res.OK {
		t.Fatalf("draft: %d %+v", code, res)
	}
	pane = f.do("GET", "/v/inbox?m="+roof, nil, map[string]string{"X-Partial": "pane"}).Body.String()
	for _, want := range []string{"Ta réponse", "Brouillon v1", "Réponse de l&#39;IA.", "Pierre Lambert &lt;pierre@example.test&gt;", "Envoyer…", "Régénérer"} {
		if !strings.Contains(pane, want) {
			t.Errorf("draft pane lacks %q", want)
		}
	}
	// Louis edits; then asks to send.
	if code, _ := f.postJSON("/brouillon", url.Values{"id": {roof}, "op": {"enregistrer"}, "body": {"Merci, vendredi 10h c'est parfait."}}); code != http.StatusOK {
		t.Fatal("save")
	}
	d, _ := f.st.ActiveDraft(t.Context(), f.ids["roof"])
	if d.Version != 2 {
		t.Fatalf("version %d", d.Version)
	}
	if rec := f.do("POST", "/envoi/preparer", url.Values{"csrf": {f.csrf}, "draft": {itoa(d.ID)}, "version": {"1"}}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("stale version accepted: %d", rec.Code)
	}
	rec := f.do("POST", "/envoi/preparer", url.Values{"csrf": {f.csrf}, "draft": {itoa(d.ID)}, "version": {"2"}}, nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Envoyer cette réponse ?") || !strings.Contains(body, "pierre@example.test") ||
		!strings.Contains(body, "vendredi 10h c&#39;est parfait") {
		t.Fatalf("confirmation: %d\n%s", rec.Code, body)
	}
	if len(f.comp.sends) != 0 {
		t.Fatal("the first click must not send")
	}
	tok := regexp.MustCompile(`name="token" value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if tok == nil {
		t.Fatal("no token")
	}
	if code, _ := f.postJSON("/envoi/confirmer", url.Values{"token": {"0000"}}); code != http.StatusConflict {
		t.Fatal("forged token accepted")
	}
	if code, res := f.postJSON("/envoi/confirmer", url.Values{"token": {tok[1]}}); code != http.StatusOK || !strings.Contains(res.Message, "envoyée") {
		t.Fatalf("send: %d %+v", code, res)
	}
	if code, _ := f.postJSON("/envoi/confirmer", url.Values{"token": {tok[1]}}); code != http.StatusConflict || len(f.comp.sends) != 1 {
		t.Fatal("a confirmation works once")
	}
}

func TestSendBlockedAndCSRF(t *testing.T) {
	f := newFixture(t)
	f.login()
	f.comp.gate = "Envoi désactivé (smtp.send_enabled: false)."
	roof := itoa(f.ids["roof"])
	if rec := f.do("POST", "/brouillon", url.Values{"csrf": {"faux"}, "id": {roof}, "op": {"nouveau"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("csrf: %d", rec.Code)
	}
	if rec := f.do("POST", "/brouillon", url.Values{"csrf": {f.csrf}, "id": {roof}, "op": {"nouveau"}}, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", rec.Code)
	}
	f.postJSON("/brouillon", url.Values{"id": {roof}, "op": {"nouveau"}})
	pane := f.do("GET", "/v/inbox?m="+roof, nil, map[string]string{"X-Partial": "pane"}).Body.String()
	if !strings.Contains(pane, "send_enabled: false") || !strings.Contains(pane, "disabled") {
		t.Fatalf("blocked send not shown:\n%s", pane)
	}
	d, _ := f.st.ActiveDraft(t.Context(), f.ids["roof"])
	if rec := f.do("POST", "/envoi/preparer", url.Values{"csrf": {f.csrf}, "draft": {itoa(d.ID)}, "version": {"1"}}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("prepare while disabled: %d", rec.Code)
	}
	if code, _ := f.postJSON("/brouillon", url.Values{"id": {roof}, "op": {"abandonner"}, "draft": {itoa(d.ID)}}); code != http.StatusOK {
		t.Fatal("cancel")
	}
	if _, err := f.st.ActiveDraft(t.Context(), f.ids["roof"]); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("draft still active")
	}
	if strings.Contains(f.do("GET", "/v/inbox?m="+itoa(f.ids["inject"]), nil, nil).Body.String(), "Répondre avec l&#39;IA") {
		t.Fatal("suspect mail offers an AI reply")
	}
}

func TestReminderButtons(t *testing.T) {
	f := newFixture(t)
	f.login()
	ctx := t.Context()
	news := f.ids["news"]
	id, _, err := f.st.InsertAction(ctx, &store.Action{MessagePK: news, Idx: 0, Text: "Lire la lettre", QuoteVerified: true, Status: store.ActionProposed})
	if err != nil {
		t.Fatal(err)
	}
	pane := f.do("GET", "/v/inbox?m="+itoa(news), nil, map[string]string{"X-Partial": "pane"}).Body.String()
	if !strings.Contains(pane, "Ajouter à Todoist") || !strings.Contains(pane, `name="action" value="`+itoa(id)+`"`) {
		t.Fatalf("no reminder buttons:\n%s", pane)
	}
	if code, _ := f.postJSON("/rappel", url.Values{"action": {itoa(id)}, "op": {"ajouter"}}); code != http.StatusOK || !f.comp.todos[id] {
		t.Fatal("add reminder")
	}
}
