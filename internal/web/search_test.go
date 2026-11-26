package web

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeAssistant struct {
	questions []string
	answer    *Answer
	err       error
}

func (a *fakeAssistant) Ask(_ context.Context, q string) (*Answer, error) {
	a.questions = append(a.questions, q)
	if a.err != nil {
		return nil, a.err
	}
	if a.answer == nil {
		return &Answer{}, nil
	}
	return a.answer, nil
}

func TestParseSearch(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Paris")
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	o, filters := parseSearch("devis de:pierre après:01/11 avant:2026-11-20 heure:10:30 avant:demain", loc, now)
	if o.Words != "devis heure:10:30 avant:demain" || o.From != "pierre" {
		t.Fatalf("%+v", o)
	}
	if !o.After.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, loc)) || !o.Before.Equal(time.Date(2026, 11, 20, 0, 0, 0, 0, loc)) {
		t.Fatalf("dates %v %v", o.After, o.Before)
	}
	if strings.Join(filters, "|") != "de pierre|depuis le 01/11/2026|avant le 20/11/2026" {
		t.Fatalf("filters %q", filters)
	}
}

func TestSearchPage(t *testing.T) {
	f := newFixture(t)
	f.login()
	page := f.do("GET", "/recherche?q="+url.QueryEscape("toiture"), nil, nil).Body.String()
	if !strings.Contains(page, "1 mail pour « toiture »") || !strings.Contains(page, "Fuite sur la toiture") ||
		strings.Contains(page, "La lettre de novembre") || !strings.Contains(page, "Demander à ta boîte") {
		t.Fatalf("search page:\n%s", page)
	}
	if !strings.Contains(page, `value="toiture"`) {
		t.Fatal("search box lost the query")
	}
	// Sender filter, and the mail opened next to the results.
	roof := itoa(f.ids["roof"])
	rec := f.do("GET", "/recherche?q="+url.QueryEscape("de:lambert")+"&m="+roof, nil, map[string]string{"X-Partial": "pane"})
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Le couvreur passe vendredi.") || !strings.Contains(body, `href="/recherche?q=de%3Alambert"`) {
		t.Fatalf("pane: %d\n%s", rec.Code, body)
	}
	// Archived mails are found, with their folder.
	code, res := f.act(url.Values{"id": {roof}, "op": {"archiver"}})
	if code != 200 || !res.OK {
		t.Fatalf("archive: %d %+v", code, res)
	}
	page = f.do("GET", "/recherche?q=toiture", nil, nil).Body.String()
	if !strings.Contains(page, "Fuite sur la toiture") || !strings.Contains(page, `class="chip where"`) {
		t.Fatalf("archived mail not found:\n%s", page)
	}
	if page := f.do("GET", "/recherche?q=le+la", nil, nil).Body.String(); !strings.Contains(page, "Rien à chercher") {
		t.Fatal("stopwords only")
	}
	// Escaped like everything else.
	if page := f.do("GET", "/recherche?q="+url.QueryEscape(`<b>x</b>`), nil, nil).Body.String(); strings.Contains(page, "<b>x</b>") {
		t.Fatal("query not escaped")
	}
}

func TestAskShowsCheckedSources(t *testing.T) {
	f := newFixture(t)
	f.login()
	roof := f.ids["roof"]
	f.ask.answer = &Answer{Text: "Le couvreur passe vendredi à 10h <script>x</script>.", Sources: []int64{roof}, Found: 3}
	if rec := f.do("POST", "/demander", url.Values{"csrf": {"faux"}, "q": {"couvreur ?"}}, nil); rec.Code != 403 || len(f.ask.questions) != 0 {
		t.Fatalf("csrf: %d", rec.Code)
	}
	rec := f.do("POST", "/demander", url.Values{"csrf": {f.csrf}, "q": {"quand passe le couvreur ?"}}, map[string]string{"X-Partial": "pane"})
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Le couvreur passe vendredi à 10h &lt;script&gt;") ||
		!strings.Contains(body, `href="/recherche?q=quand&#43;passe&#43;le&#43;couvreur&#43;%3F&amp;m=`+itoa(roof)+`"`) ||
		!strings.Contains(body, "Fuite sur la toiture") || !strings.Contains(body, "3 mails trouvés") {
		t.Fatalf("answer: %d\n%s", rec.Code, body)
	}
	// Without JavaScript: the whole page, results on the left.
	page := f.do("POST", "/demander", url.Values{"csrf": {f.csrf}, "q": {"couvreur"}}, nil).Body.String()
	if !strings.Contains(page, `class="mail answer"`) || !strings.Contains(page, `class="items"`) {
		t.Fatalf("full page:\n%s", page)
	}
	f.ask.answer = &Answer{}
	if body := f.do("POST", "/demander", url.Values{"csrf": {f.csrf}, "q": {"rien"}}, map[string]string{"X-Partial": "pane"}).Body.String(); !strings.Contains(body, "n'a pas été interrogée") {
		t.Fatalf("no hits:\n%s", body)
	}
	f.ask.err = errors.New("une question est déjà en cours")
	if body := f.do("POST", "/demander", url.Values{"csrf": {f.csrf}, "q": {"x"}}, map[string]string{"X-Partial": "pane"}).Body.String(); !strings.Contains(body, "déjà en cours") {
		t.Fatalf("error:\n%s", body)
	}
}

func TestTodayPage(t *testing.T) {
	f := newFixture(t)
	f.login()
	page := f.do("GET", "/aujourdhui", nil, nil).Body.String()
	for _, want := range []string{"Aujourd'hui", "jeudi 26 novembre", "Ce qui compte", "Fuite sur la toiture",
		"Confirmer le couvreur", "hui à 18h", "Arrivés aujourd'hui", "Pas encore analysé", "Prochain résumé sur Telegram : 18:00",
		`class="has-pane wide"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("missing %q:\n%s", want, page)
		}
	}
	// The suspect mail is never in « Ce qui compte ».
	sec := page[strings.Index(page, "Ce qui compte"):strings.Index(page, "Arrivés aujourd")]
	if strings.Contains(sec, "Vérification") {
		t.Fatal("suspect mail among the important ones")
	}
	if rec := f.do("GET", "/aujourdhui", nil, map[string]string{"X-Partial": "pane"}); !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), `<article class="mail today"`) {
		t.Fatalf("partial:\n%s", rec.Body.String())
	}
}
