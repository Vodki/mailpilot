package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

var jsonHdr = map[string]string{"Accept": "application/json", "Sec-Fetch-Site": "same-origin"}

func (f *fixture) act(fields url.Values) (int, actionResult) {
	f.t.Helper()
	if fields.Get("csrf") == "" {
		fields.Set("csrf", f.csrf)
	}
	rec := f.do("POST", "/action", fields, jsonHdr)
	var res actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil && rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
		f.t.Fatalf("action %v: %d %q", fields, rec.Code, rec.Body.String())
	}
	return rec.Code, res
}

func (f *fixture) inboxHas(subject string) bool {
	f.t.Helper()
	body := f.do("GET", "/v/inbox", nil, map[string]string{"X-Partial": "list"}).Body.String()
	return strings.Contains(body, `<span class="subject">`+subject)
}

func TestOpeningMarksReadOnlyOnLouisRequest(t *testing.T) {
	f := newFixture(t)
	f.login()
	roof := itoa(f.ids["roof"])
	pane := f.do("GET", "/v/inbox?m="+roof, nil, nil).Body.String()
	if f.mb.writes != 0 {
		t.Fatal("displaying a mail changed the mailbox")
	}
	if !strings.Contains(pane, "data-mark-on-open") || !strings.Contains(pane, `value="lu"`) {
		t.Fatalf("unread mail must offer mark-on-open:\n%s", pane)
	}
	code, res := f.act(url.Values{"op": {"lu"}, "id": {roof}})
	if code != http.StatusOK || !res.OK || res.Gone || f.mb.writes != 1 || !f.mb.folders["INBOX"].msgs[1].seen {
		t.Fatalf("mark read: %d %+v writes %d", code, res, f.mb.writes)
	}
	pane = f.do("GET", "/v/inbox?m="+roof, nil, map[string]string{"X-Partial": "pane"}).Body.String()
	if strings.Contains(pane, "data-mark-on-open") || !strings.Contains(pane, `value="nonlu"`) {
		t.Fatalf("read mail:\n%s", pane)
	}
	if strings.Contains(f.do("GET", "/v/inbox", nil, nil).Body.String(), `class="item unread`) {
		t.Fatal("list still shows it unread")
	}
	if code, _ := f.act(url.Values{"op": {"nonlu"}, "id": {roof}}); code != http.StatusOK || f.mb.folders["INBOX"].msgs[1].seen {
		t.Fatal("mark unread failed")
	}

	f.srv.cfg.Web.MarkReadOnOpen = false
	if pane := f.do("GET", "/v/inbox?m="+roof, nil, nil).Body.String(); strings.Contains(pane, "data-mark-on-open") {
		t.Fatal("mark_read_on_open: false ignored")
	}
}

func TestArchiveTrashAndUndo(t *testing.T) {
	f := newFixture(t)
	f.login()
	news, roof := itoa(f.ids["news"]), itoa(f.ids["roof"])

	code, res := f.act(url.Values{"op": {"archiver"}, "id": {news}})
	if code != http.StatusOK || !res.Gone || res.Undo == nil || res.Undo.ID != f.ids["news"] || res.Undo.Dest != "INBOX" ||
		!strings.Contains(res.Message, "Archive") {
		t.Fatalf("archive: %d %+v", code, res)
	}
	if f.inboxHas("La lettre de novembre") || len(f.mb.folders["Archive"].msgs) != 1 {
		t.Fatal("archived mail still in the inbox")
	}
	m, _ := f.st.GetMessage(context.Background(), f.ids["news"])
	if m.Mailbox != "Archive" || m.UID != 1 || m.UIDValidity != 8 {
		t.Fatalf("store not relocated: %s %d %d", m.Mailbox, m.UIDValidity, m.UID)
	}

	if code, res := f.act(url.Values{"op": {"corbeille"}, "id": {roof}}); code != http.StatusOK || !strings.Contains(res.Message, "corbeille") {
		t.Fatalf("trash: %d %+v", code, res)
	}
	if code, res := f.act(url.Values{"op": {"corbeille"}, "id": {roof}}); code != http.StatusConflict || !strings.Contains(res.Message, "jamais") {
		t.Fatalf("trash twice must not delete for good: %d %+v", code, res)
	}
	// Undo, as the toast does.
	if code, res := f.act(url.Values{"op": {"deplacer"}, "id": {roof}, "dest": {"INBOX"}}); code != http.StatusOK || !strings.Contains(res.Message, "boîte de réception") {
		t.Fatalf("undo: %d %+v", code, res)
	}
	if !f.inboxHas("Fuite sur la toiture") || len(f.mb.folders["Trash"].msgs) != 0 {
		t.Fatal("undo did not bring it back")
	}
	if code, _ := f.act(url.Values{"op": {"deplacer"}, "id": {roof}, "dest": {"Nulle part"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown folder: %d", code)
	}
	// No JavaScript: a plain form post comes back to the list.
	rec := f.do("POST", "/action", url.Values{"csrf": {f.csrf}, "op": {"deplacer"}, "id": {roof}, "dest": {"Factures"}, "back": {"/v/urgent"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/v/urgent" {
		t.Fatalf("form post: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = f.do("POST", "/action", url.Values{"csrf": {f.csrf}, "op": {"lu"}, "id": {roof}, "back": {"//evil.test/"}}, nil)
	if rec.Header().Get("Location") != "/v/inbox" {
		t.Fatalf("open redirect: %s", rec.Header().Get("Location"))
	}
}

func TestActionsNeedCSRFAndSameOrigin(t *testing.T) {
	f := newFixture(t)
	f.login()
	id := itoa(f.ids["roof"])
	if code, _ := f.act(url.Values{"op": {"corbeille"}, "id": {id}, "csrf": {"faux"}}); code != http.StatusForbidden {
		t.Fatalf("bad csrf: %d", code)
	}
	rec := f.do("POST", "/action", url.Values{"csrf": {f.csrf}, "op": {"corbeille"}, "id": {id}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", rec.Code)
	}
	if code, _ := f.act(url.Values{"op": {"effacer"}, "id": {id}}); code != http.StatusBadRequest {
		t.Fatalf("unknown op: %d", code)
	}
	if rec := f.do("GET", "/action?op=corbeille&id="+id, nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must not act: %d", rec.Code)
	}
	if f.mb.writes != 0 {
		t.Fatal("a refused request changed the mailbox")
	}
}

func TestMovedElsewhere(t *testing.T) {
	f := newFixture(t)
	f.login()
	if !f.inboxHas("La lettre de novembre") {
		t.Fatal("setup")
	}
	// Another client deletes the newsletter.
	delete(f.mb.folders["INBOX"].msgs, 2)
	if code, res := f.act(url.Values{"op": {"lu"}, "id": {itoa(f.ids["news"])}}); code != http.StatusConflict || !res.Gone {
		t.Fatalf("stale action: %d %+v", code, res)
	}
	f.now = f.now.Add(16 * time.Second)
	if f.inboxHas("La lettre de novembre") {
		t.Fatal("a mail gone from the server is still listed")
	}
	if !f.inboxHas("Fuite sur la toiture") {
		t.Fatal("other mails hidden")
	}
}

func TestFolderPage(t *testing.T) {
	f := newFixture(t)
	f.login()
	uid := f.mb.add("Factures", &fakeMsg{id: "edf@t", from: "factures@edf.test", subject: "Votre facture de novembre", body: "Montant : 84 €"})
	if code, _ := f.act(url.Values{"op": {"archiver"}, "id": {itoa(f.ids["news"])}}); code != http.StatusOK {
		t.Fatal("archive")
	}
	writes := f.mb.writes

	inbox := f.do("GET", "/v/inbox", nil, nil).Body.String()
	if !strings.Contains(inbox, `href="/dossier?f=Factures"`) || !strings.Contains(inbox, ">Archives</span>") || !strings.Contains(inbox, ">Corbeille</span>") {
		t.Fatalf("folders missing from the menu:\n%s", inbox)
	}
	list := f.do("GET", "/dossier?f=Factures", nil, nil).Body.String()
	if !strings.Contains(list, "Votre facture de novembre") || !strings.Contains(list, `class="item unread`) {
		t.Fatalf("folder list:\n%s", list)
	}
	archive := f.do("GET", "/dossier?f=Archive", nil, nil).Body.String()
	if !strings.Contains(archive, "Promotions du mois.") {
		t.Fatal("an archived mail lost its AI summary")
	}
	pane := f.do("GET", "/dossier?f=Factures&v=10&u="+itoa(int64(uid)), nil, map[string]string{"X-Partial": "pane"}).Body.String()
	if !strings.Contains(pane, "Montant : 84 €") || !strings.Contains(pane, `name="f" value="Factures"`) || !strings.Contains(pane, "data-mark-on-open") {
		t.Fatalf("folder pane:\n%s", pane)
	}
	if f.mb.writes != writes || f.mb.folders["Factures"].msgs[uid].seen {
		t.Fatal("reading a folder changed the mailbox")
	}
	pane = f.do("GET", "/dossier?f=Factures&v=99&u=1", nil, map[string]string{"X-Partial": "pane"}).Body.String()
	if !strings.Contains(pane, "n'est plus dans ce dossier") || strings.Contains(pane, "toolbar") {
		t.Fatalf("stale location:\n%s", pane)
	}
	// Acting from the folder page targets that exact place.
	code, res := f.act(url.Values{"op": {"corbeille"}, "f": {"Factures"}, "v": {"10"}, "u": {itoa(int64(uid))}})
	if code != http.StatusOK || res.Undo == nil || res.Undo.Folder != "Trash" || res.Undo.Dest != "Factures" {
		t.Fatalf("trash from folder: %d %+v", code, res)
	}
	if rec := f.do("GET", "/dossier?f=Inconnu", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown folder: %d", rec.Code)
	}
	if rec := f.do("GET", "/dossier?f=INBOX", nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("inbox folder: %d", rec.Code)
	}
}

func TestPurgedBodyIsReadFromServer(t *testing.T) {
	f := newFixture(t)
	f.login()
	if _, err := f.st.PurgeBodies(context.Background(), f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pane := f.do("GET", "/v/inbox?m="+itoa(f.ids["roof"]), nil, map[string]string{"X-Partial": "pane"}).Body.String()
	if !strings.Contains(pane, "Le couvreur passe vendredi.") || !strings.Contains(pane, "relu sur le serveur") {
		t.Fatalf("purged body:\n%s", pane)
	}
	m, _ := f.st.GetMessage(context.Background(), f.ids["roof"])
	if !m.BodyPurged {
		t.Fatal("the re-read body must not be stored again")
	}
	if f.mb.writes != 0 {
		t.Fatal("re-reading changed the mailbox")
	}
}
