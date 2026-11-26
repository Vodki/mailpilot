package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/parse"
	"github.com/Vodki/mailpilot/internal/store"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func constantEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

var funcs = template.FuncMap{
	"initial": func(a addrVM) string {
		n := strings.TrimSpace(a.Name)
		if n == "" {
			n = a.Email
		}
		for _, r := range n {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
}

var viewTitles = map[string]string{
	store.ViewInbox:       "Boîte de réception",
	store.ViewUrgent:      "Urgent",
	store.ViewTodo:        "À traiter",
	store.ViewReply:       "Réponse attendue",
	store.ViewBills:       "Factures",
	store.ViewNewsletters: "Newsletters",
	store.ViewSuspect:     "Suspects",
}

var viewIcons = map[string]string{
	store.ViewInbox: "inbox", store.ViewUrgent: "flame", store.ViewTodo: "check", store.ViewReply: "reply",
	store.ViewBills: "receipt", store.ViewNewsletters: "news", store.ViewSuspect: "shield",
}

var prioLabels = map[string]string{"urgent": "Urgent", "haute": "Important", "normale": "Normal", "basse": "Faible"}

var catLabels = map[string]string{
	"action_requise": "Action requise", "reponse_attendue": "Réponse attendue", "rendez_vous": "Rendez-vous",
	"facture_paiement": "Facture", "information": "Information", "newsletter": "Newsletter",
	"notification_auto": "Notification", "personnel": "Personnel", "spam_probable": "Spam probable", "autre": "Autre",
}

var frMonthsShort = []string{"", "janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.", "déc."}
var frDays = []string{"dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"}
var frMonths = []string{"", "janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"}

type viewLink struct {
	Name, Title, Icon string
	Count             int
	Active, AI        bool
}

type itemVM struct {
	ID                          int64
	Href                        string
	FromName                    string
	Subject                     string
	Time                        string
	Unread, Selected            bool
	Prio, PrioLabel, CatLabel   string
	Summary                     string
	Pending                     bool // not analysed yet
	Suspect, Attach, Draft      bool
	TasksCreated, TasksProposed int
}

type actionVM struct {
	ID                             int64
	Text, Due, Status, StatusClass string
	Unverified                     bool
	Decide                         bool // proposed or failed: Louis can add or dismiss it
}

type addrVM struct{ Name, Email string }

type threadVM struct {
	ID             int64
	Href           string
	FromName, Time string
	Summary, Subj  string
	Current        bool
}

type paneVM struct {
	ID                   int64 // stored message (0: read from a folder only)
	Loc                  Loc
	Unread               bool
	MarkOnOpen           bool // Louis's open marks it read
	ActionsOn            bool
	InTrash, InArchive   bool
	Moves                []folderLink // destinations of "Déplacer"
	CSRF                 string
	ListHref             string
	FromServer           bool // body re-read from the server (purged or unknown)
	Missing              bool // the server no longer has it at this place
	ByLoc                bool // actions target Loc (folder page), not the stored id
	CanReply             bool
	Draft                *draftVM
	From                 addrVM
	To, Cc               []addrVM
	Date                 string
	Subject              string
	Body                 string
	BodyPurged, HTMLOnly bool
	Attachments          []string
	Analyzed             bool
	Failed               bool
	Prio, PrioLabel      string
	CatLabel             string
	Summary              string
	Reason               string
	NeedsReply           bool
	Suspect              bool
	Actions              []actionVM
	Thread               []threadVM
	Model                string
	BackHref             string
}

type pageVM struct {
	Title, View, ViewTitle string
	Views                  []viewLink
	Items                  []itemVM
	Pane                   *paneVM
	CSRF                   string
	Account                string
	Version                string
	SendEnabled            bool
	MoreHref               string
	Folders                []folderLink
	Folder                 string // folder page: its name
	Composer               *composerVM
	CanCompose             bool
}

type folderLink struct {
	Name, Href, Icon, Title string
	Active                  bool
}

var folderIcons = map[string]string{"archive": "archive", "trash": "trash", "sent": "send", "drafts": "draft", "junk": "shield", "": "folder"}

func (s *Server) folderLinks(r *http.Request, active string) []folderLink {
	var out []folderLink
	for _, f := range s.folders(r.Context()) {
		if f.Role == "inbox" || f.Name == s.cfg.IMAP.Mailbox {
			continue
		}
		out = append(out, folderLink{Name: f.Name, Title: folderTitle(f), Href: "/dossier?f=" + url.QueryEscape(f.Name),
			Icon: folderIcons[f.Role], Active: f.Name == active})
	}
	return out
}

var roleTitles = map[string]string{"inbox": "Boîte de réception", "archive": "Archives", "trash": "Corbeille", "sent": "Envoyés", "drafts": "Brouillons", "junk": "Indésirables"}

// folderTitle names a folder: the server's name, except special folders
// with an English name.
func folderTitle(f Folder) string {
	if t, ok := roleTitles[f.Role]; ok && isASCIIName(f.Name) {
		return t
	}
	return f.Name
}

func isASCIIName(n string) bool {
	for _, r := range n {
		if r > 127 {
			return false
		}
	}
	return true
}

func (s *Server) fmtListTime(t time.Time) string {
	loc := s.cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	now := s.now().In(loc)
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04")
	case y1 == y2:
		return strconv.Itoa(d1) + " " + frMonthsShort[m1]
	default:
		return t.Format("02/01/2006")
	}
}

func (s *Server) fmtFullTime(t time.Time) string {
	loc := s.cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	return frDays[t.Weekday()] + " " + strconv.Itoa(t.Day()) + " " + frMonths[t.Month()] + " " + strconv.Itoa(t.Year()) + ", " + t.Format("15:04")
}

func displayName(a store.Address) string {
	if strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	if a.Email != "" {
		return a.Email
	}
	return "(expéditeur inconnu)"
}

func orNoSubject(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(sans objet)"
	}
	return s
}

func (s *Server) viewPage(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	ctx := r.Context()
	view := r.PathValue("view")
	if _, ok := viewTitles[view]; !ok {
		http.NotFound(w, r)
		return
	}
	mailbox := s.cfg.IMAP.Mailbox
	before, _ := strconv.ParseInt(r.URL.Query().Get("avant"), 10, 64)
	selected, _ := strconv.ParseInt(r.URL.Query().Get("m"), 10, 64)
	partial := r.Header.Get("X-Partial")

	page := pageVM{View: view, ViewTitle: viewTitles[view], CSRF: ws.CSRF, Account: s.cfg.IMAP.Username,
		Version: s.version, SendEnabled: s.cfg.SMTP.SendEnabled, CanCompose: s.comp != nil}
	page.Title = page.ViewTitle + " · mailpilot"

	if partial == "" || partial == "list" {
		const pageSize = 50
		// First: it also hides the mails moved or deleted elsewhere.
		val, unread := s.inboxStatus(ctx)
		items, err := s.st.ListMessages(ctx, store.ListOptions{Mailbox: mailbox, View: view, Limit: pageSize + 1, BeforeID: before})
		if err != nil {
			s.fail(w, "list", err)
			return
		}
		if len(items) > pageSize {
			items = items[:pageSize]
			page.MoreHref = "/v/" + view + "?avant=" + itoa(items[len(items)-1].ID)
		}
		for _, it := range items {
			vm := itemVM{ID: it.ID, Href: "/v/" + view + "?m=" + itoa(it.ID), FromName: displayName(it.From),
				Subject: orNoSubject(it.Subject), Time: s.fmtListTime(it.ReceivedAt), Selected: it.ID == selected,
				Unread: unread != nil && it.UIDValidity == val && unread[it.UID], Attach: it.HasAttachments,
				Draft: it.HasDraft, TasksCreated: it.TasksCreated, TasksProposed: it.TasksProposed,
				Suspect: it.AIInstructions || it.Category == "spam_probable"}
			if it.Analyzed {
				vm.Prio, vm.PrioLabel, vm.CatLabel, vm.Summary = it.Priority, prioLabels[it.Priority], catLabels[it.Category], it.Summary
			} else {
				vm.Pending = it.State == store.StateNew
			}
			page.Items = append(page.Items, vm)
		}
		counts, err := s.st.ViewCounts(ctx, 1, mailbox)
		if err != nil {
			s.fail(w, "counts", err)
			return
		}
		page.Views = viewLinks(counts, view)
		page.Folders = s.folderLinks(r, "")
	}
	if selected != 0 && (partial == "" || partial == "pane") {
		pane, err := s.pane(r, selected, view, ws)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.fail(w, "pane", err)
			return
		}
		page.Pane = pane
		page.Title = pane.Subject + " · mailpilot"
	}
	switch partial {
	case "list":
		s.render(w, "list", page)
	case "pane":
		s.render(w, "pane", page)
	default:
		s.render(w, "app.html", page)
	}
}

func viewLinks(counts map[string]int, active string) []viewLink {
	var out []viewLink
	for _, v := range store.Views {
		out = append(out, viewLink{Name: v, Title: viewTitles[v], Icon: viewIcons[v], Count: counts[v],
			Active: v == active, AI: v != store.ViewInbox})
	}
	return out
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("web "+what, "err", err)
	http.Error(w, "erreur interne", http.StatusInternalServerError)
}

// triageView is the subset of the stored analysis JSON the pane shows.
type triageView struct {
	NeedsReply bool   `json:"needs_reply"`
	Reason     string `json:"reason"`
	Actions    []struct {
		Owner string `json:"owner"`
	} `json:"actions"`
}

func addrs(in []store.Address) []addrVM {
	out := make([]addrVM, 0, len(in))
	for _, a := range in {
		out = append(out, addrVM{Name: a.Name, Email: a.Email})
	}
	return out
}

var actionStatus = map[string][2]string{
	store.ActionCreated:   {"Ajouté à Todoist", "ok"},
	store.ActionCreating:  {"Envoi à Todoist…", "wait"},
	store.ActionProposed:  {"À confirmer", "wait"},
	store.ActionFailed:    {"Échec Todoist", "bad"},
	store.ActionDismissed: {"Ignoré", "off"},
}

func (s *Server) pane(r *http.Request, id int64, view string, ws *store.WebSession) (*paneVM, error) {
	ctx := r.Context()
	m, err := s.st.GetMessage(ctx, id)
	if err != nil {
		return nil, err
	}
	p := &paneVM{ID: m.ID, From: addrVM{Name: m.From.Name, Email: m.From.Email}, To: addrs(m.To), Cc: addrs(m.Cc),
		Date: s.fmtFullTime(m.ReceivedAt), Subject: orNoSubject(m.Subject), Body: m.TextFull,
		BodyPurged: m.BodyPurged, HTMLOnly: m.HTMLOnly, BackHref: "/v/" + view, ListHref: "/v/" + view,
		Failed: m.State == store.StateAnalysisFailed,
		Loc:    Loc{Folder: m.Mailbox, UIDValidity: m.UIDValidity, UID: m.UID}}
	if p.Body == "" {
		p.Body = m.TextClean
	}
	for _, a := range m.Attachments {
		if !a.Inline {
			p.Attachments = append(p.Attachments, a.Filename)
		}
	}
	if m.BodyPurged && s.mb != nil {
		// Kept out of the store on purpose (retention): shown, not saved.
		if pm, err := s.readFromServer(ctx, p.Loc); err == nil {
			p.Body, p.HTMLOnly, p.BodyPurged, p.FromServer = pm.TextFull, pm.HTMLOnly, false, true
		}
	}
	if m.Mailbox == s.cfg.IMAP.Mailbox {
		val, unread := s.inboxStatus(ctx)
		p.Unread = unread != nil && val == m.UIDValidity && unread[m.UID]
	}
	if err := s.fillAI(ctx, p, id); err != nil {
		return nil, err
	}
	s.fillActions(r, p, ws)
	if err := s.fillDraft(ctx, p); err != nil {
		return nil, err
	}
	if m.ThreadID != 0 {
		thread, err := s.st.ThreadMessages(ctx, m.ThreadID, 20)
		if err != nil {
			return nil, err
		}
		if len(thread) > 1 {
			for _, t := range thread {
				tv := threadVM{ID: t.ID, Href: "/v/" + view + "?m=" + itoa(t.ID), FromName: displayName(t.From),
					Time: s.fmtListTime(t.ReceivedAt), Subj: orNoSubject(t.Subject), Current: t.ID == m.ID}
				if ta, err := s.st.LatestAnalysis(ctx, t.ID); err == nil {
					tv.Summary = ta.Summary
				}
				p.Thread = append(p.Thread, tv)
			}
		}
	}
	return p, nil
}

// fillAI adds the stored analysis and actions of message id to the pane.
func (s *Server) fillAI(ctx context.Context, p *paneVM, id int64) error {
	a, err := s.st.LatestAnalysis(ctx, id)
	switch {
	case err == nil:
		var tv triageView
		_ = json.Unmarshal([]byte(a.ResultJSON), &tv)
		p.Analyzed = true
		p.Prio, p.PrioLabel, p.CatLabel = a.Priority, prioLabels[a.Priority], catLabels[a.Category]
		p.Summary, p.Reason, p.NeedsReply, p.Model = a.Summary, tv.Reason, tv.NeedsReply, a.Model
		p.Suspect = a.AIInstructions || a.Category == "spam_probable"
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	acts, err := s.st.MessageActions(ctx, id)
	if err != nil {
		return err
	}
	for _, x := range acts {
		st := actionStatus[x.Status]
		vm := actionVM{ID: x.ID, Text: x.Text, Status: st[0], StatusClass: st[1], Unverified: !x.QuoteVerified,
			Decide: s.comp != nil && (x.Status == store.ActionProposed || x.Status == store.ActionFailed)}
		if x.DueDate != "" {
			if d, err := time.Parse("2006-01-02", x.DueDate); err == nil {
				vm.Due = strconv.Itoa(d.Day()) + " " + frMonthsShort[d.Month()]
				if x.DueTime != "" {
					vm.Due += " à " + strings.TrimSuffix(strings.Replace(x.DueTime, ":", "h", 1), "00")
				}
			}
		}
		p.Actions = append(p.Actions, vm)
	}
	return nil
}

// fillActions prepares the toolbar: where the mail can go from here.
func (s *Server) fillActions(r *http.Request, p *paneVM, ws *store.WebSession) {
	p.CSRF = ws.CSRF
	if s.mb == nil || p.Loc.UID == 0 || p.Missing {
		return
	}
	p.ActionsOn = true
	p.MarkOnOpen = s.cfg.Web.MarkReadOnOpen && p.Unread
	for _, f := range s.folders(r.Context()) {
		if f.Name == p.Loc.Folder {
			p.InTrash, p.InArchive = f.Role == "trash", f.Role == "archive"
			continue
		}
		if f.Role == "drafts" || f.Role == "sent" {
			continue
		}
		p.Moves = append(p.Moves, folderLink{Name: f.Name, Title: folderTitle(f)})
	}
}

func (s *Server) readFromServer(ctx context.Context, at Loc) (*parse.Message, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.mb.Fetch(cctx, at)
	if err != nil {
		return nil, err
	}
	return parse.Parse(raw)
}
