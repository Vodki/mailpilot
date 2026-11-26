package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/store"
)

// New messages and forwards. Recipients are typed by Louis; the Composer
// parses them strictly and sends with the same gates as a reply.

// ComposeRequest saves a version of a new message or forward.
type ComposeRequest struct {
	ID      int64 // 0: new composition
	Forward int64 // forwarded stored message (new composition only)
	To, Cc  string
	Subject string
	Body    string
}

const cbWebSendNew = "web_send_new"

type composerVM struct {
	ID, Forward int64
	Version     int
	To, Cc      string
	Subject     string
	Body        string
	InDrafts    bool
	Sent        bool
	SendBlocked string
	CSRF        string
	Error       string
	Source      *paneVM // forwarded mail, shown below
	Drafts      []compLink
}

type compLink struct {
	Href, Title, To, When string
	Current               bool
}

// composePage shows the editor: a new message, a forward (?transferer=id)
// or a saved composition (?c=id), next to the inbox.
func (s *Server) composePage(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if s.comp == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	vm := &composerVM{CSRF: ws.CSRF, SendBlocked: s.comp.SendGate(ctx), Error: q.Get("erreur")}
	if id, _ := strconv.ParseInt(q.Get("c"), 10, 64); id > 0 {
		c, err := s.st.GetComposition(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.fail(w, "composition", err)
			return
		}
		vm.ID, vm.Version, vm.To, vm.Cc, vm.Subject, vm.Body = c.ID, c.Version, addrText(c.To), addrText(c.Cc), c.Subject, c.Body
		vm.InDrafts, vm.Sent, vm.Forward = c.IMAPFolder != "", c.Status == "sent", c.SourcePK
		if c.Status == "cancelled" {
			http.Redirect(w, r, "/ecrire", http.StatusSeeOther)
			return
		}
	} else if fwd, _ := strconv.ParseInt(q.Get("transferer"), 10, 64); fwd > 0 {
		m, err := s.st.GetMessage(ctx, fwd)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		vm.Forward, vm.Subject = fwd, forwardSubject(m.Subject)
	}
	if vm.Forward > 0 {
		if p, err := s.pane(r, vm.Forward, store.ViewInbox, ws); err == nil {
			vm.Source = p
		}
	}
	if list, err := s.st.ActiveCompositions(ctx, 20); err == nil {
		for _, c := range list {
			title := c.Subject
			if strings.TrimSpace(title) == "" {
				title = "(sans objet)"
			}
			vm.Drafts = append(vm.Drafts, compLink{Href: "/ecrire?c=" + itoa(c.ID), Title: title, To: addrText(c.To),
				When: s.fmtListTime(c.UpdatedAt), Current: c.ID == vm.ID})
		}
	}
	if r.Header.Get("X-Partial") == "pane" {
		s.render(w, "composer", vm)
		return
	}
	// The page: the inbox on the left, the editor as the pane.
	r2 := r.Clone(ctx)
	r2.URL.RawQuery = ""
	page, ok := s.inboxPage(w, r2, ws)
	if !ok {
		return
	}
	page.Composer = vm
	page.Title = "Nouveau message · mailpilot"
	if vm.Forward > 0 {
		page.Title = "Transférer · mailpilot"
	}
	s.render(w, "app.html", page)
}

func forwardSubject(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"tr:", "tr :", "fwd:", "fw:"} {
		if strings.HasPrefix(l, p) {
			return strings.TrimSpace(s)
		}
	}
	return "TR: " + strings.TrimSpace(s)
}

// composeSave stores a version (and its copy in Drafts) or drops it.
func (s *Server) composeSave(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	if s.comp == nil {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("c"), 10, 64)
	fwd, _ := strconv.ParseInt(r.PostFormValue("transferer"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if r.PostFormValue("op") == "abandonner" {
		if err := s.comp.CancelComposition(ctx, id); err != nil {
			s.actionReply(w, r, http.StatusConflict, actionResult{Message: "Impossible : " + err.Error()})
			return
		}
		s.composeReply(w, r, actionResult{OK: true, Message: "Brouillon supprimé (et retiré du dossier Brouillons)"}, "/v/inbox")
		return
	}
	req := ComposeRequest{ID: id, Forward: fwd, To: r.PostFormValue("to"), Cc: r.PostFormValue("cc"),
		Subject: r.PostFormValue("subject"), Body: strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n")}
	if len(req.Body) > maxBody || len(req.Subject) > 500 || len(req.To)+len(req.Cc) > 4000 {
		s.actionReply(w, r, http.StatusRequestEntityTooLarge, actionResult{Message: "Texte trop long."})
		return
	}
	newID, err := s.comp.Compose(ctx, req)
	if err != nil {
		if !strings.Contains(r.Header.Get("Accept"), "application/json") {
			// No JavaScript: back to the form, which keeps the last saved version.
			back := "/ecrire?erreur=" + urlQuery(err.Error())
			if id > 0 {
				back += "&c=" + itoa(id)
			}
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		s.actionReply(w, r, http.StatusConflict, actionResult{Message: err.Error()})
		return
	}
	s.composeReply(w, r, actionResult{OK: true, Message: "Brouillon enregistré dans Brouillons"}, "/ecrire?c="+itoa(newID))
}

func (s *Server) composeReply(w http.ResponseWriter, r *http.Request, res actionResult, next string) {
	res.Redirect = next
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.actionReply(w, r, http.StatusOK, res)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// inboxPage builds the inbox list and navigation around another pane.
func (s *Server) inboxPage(w http.ResponseWriter, r *http.Request, ws *store.WebSession) (pageVM, bool) {
	ctx := r.Context()
	page := pageVM{View: "", ViewTitle: viewTitles[store.ViewInbox], CSRF: ws.CSRF, Account: s.cfg.IMAP.Username,
		Version: s.version, SendEnabled: s.cfg.SMTP.SendEnabled, CanCompose: s.comp != nil}
	val, unread := s.inboxStatus(ctx)
	items, err := s.st.ListMessages(ctx, store.ListOptions{Mailbox: s.cfg.IMAP.Mailbox, View: store.ViewInbox, Limit: 50})
	if err != nil {
		s.fail(w, "list", err)
		return page, false
	}
	for _, it := range items {
		vm := s.item(it, "/v/inbox?m="+itoa(it.ID), val, unread)
		page.Items = append(page.Items, vm)
	}
	counts, err := s.st.ViewCounts(ctx, 1, s.cfg.IMAP.Mailbox)
	if err != nil {
		s.fail(w, "counts", err)
		return page, false
	}
	page.Views = viewLinks(counts, "")
	page.Folders = s.folderLinks(r, "")
	return page, true
}

func urlQuery(v string) string { return url.QueryEscape(v) }
