package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestNewMessageFlow(t *testing.T) {
	f := newFixture(t)
	f.login()
	page := f.do("GET", "/ecrire", nil, nil).Body.String()
	if !strings.Contains(page, `name="to"`) || !strings.Contains(page, "Fuite sur la toiture") || !strings.Contains(page, `class="new-mail"`) {
		t.Fatalf("composer page:\n%s", page)
	}
	// Plain form post (no JavaScript): back to the editor of the saved draft.
	rec := f.do("POST", "/ecrire", url.Values{"csrf": {f.csrf}, "to": {"marie@example.test"}, "subject": {"Dîner"}, "body": {"Samedi ?"}}, nil)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/ecrire?c=") {
		t.Fatalf("save: %d %s", rec.Code, loc)
	}
	id := strings.TrimPrefix(loc, "/ecrire?c=")
	rec = f.do("POST", "/ecrire", url.Values{"csrf": {f.csrf}, "c": {id}, "to": {"pas une adresse"}}, nil)
	if !strings.Contains(rec.Header().Get("Location"), "erreur=") {
		t.Fatalf("invalid address: %s", rec.Header().Get("Location"))
	}
	page = f.do("GET", "/ecrire?c="+id, nil, nil).Body.String()
	if !strings.Contains(page, "Brouillon v1") || !strings.Contains(page, `value="Dîner"`) || !strings.Contains(page, "Envoyer…") || !strings.Contains(page, "Brouillons en cours") {
		t.Fatalf("saved draft:\n%s", page)
	}
	if rec := f.do("POST", "/ecrire", url.Values{"csrf": {"faux"}, "to": {"a@b.test"}}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("csrf: %d", rec.Code)
	}

	prep := f.do("POST", "/envoi/preparer", url.Values{"csrf": {f.csrf}, "kind": {"c"}, "draft": {id}, "version": {"1"}}, nil)
	body := prep.Body.String()
	if prep.Code != http.StatusOK || !strings.Contains(body, "Envoyer ce message ?") || !strings.Contains(body, "marie@example.test") {
		t.Fatalf("prepare: %d\n%s", prep.Code, body)
	}
	tok := regexp.MustCompile(`name="token" value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if len(f.comp.sends) != 0 || tok == nil {
		t.Fatal("first click sent, or no token")
	}
	code, res := f.postJSON("/envoi/confirmer", url.Values{"token": {tok[1]}})
	if code != http.StatusOK || res.Message != "Message envoyé" || res.Redirect != "/v/inbox" || len(f.comp.sends) != 1 {
		t.Fatalf("send: %d %+v", code, res)
	}
	if !strings.Contains(f.do("GET", "/ecrire?c="+id, nil, nil).Body.String(), "Message envoyé") {
		t.Fatal("sent composition still editable")
	}
}

func TestForwardFlow(t *testing.T) {
	f := newFixture(t)
	f.login()
	roof := itoa(f.ids["roof"])
	if !strings.Contains(f.do("GET", "/v/inbox?m="+roof, nil, nil).Body.String(), `href="/ecrire?transferer=`+roof+`"`) {
		t.Fatal("no forward link")
	}
	page := f.do("GET", "/ecrire?transferer="+roof, nil, nil).Body.String()
	if !strings.Contains(page, `value="TR: Fuite sur la toiture"`) || !strings.Contains(page, "Le couvreur passe vendredi.") ||
		!strings.Contains(page, `name="transferer" value="`+roof+`"`) {
		t.Fatalf("forward page:\n%s", page)
	}
	rec := f.do("POST", "/ecrire", url.Values{"csrf": {f.csrf}, "transferer": {roof}, "to": {"compta@example.test"}, "subject": {"TR: Fuite"}}, nil)
	id := strings.TrimPrefix(rec.Header().Get("Location"), "/ecrire?c=")
	c, err := f.st.GetComposition(t.Context(), mustInt(t, id))
	if err != nil || c.SourcePK != f.ids["roof"] {
		t.Fatalf("forward saved: %+v %v", c, err)
	}
	if rec := f.do("POST", "/ecrire", url.Values{"csrf": {f.csrf}, "c": {id}, "op": {"abandonner"}}, nil); rec.Header().Get("Location") != "/v/inbox" {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}
