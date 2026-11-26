package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	Text, Due, Status, StatusClass string
	Unverified                     bool
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
	ID                   int64
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
		Version: s.version, SendEnabled: s.cfg.SMTP.SendEnabled}
	page.Title = page.ViewTitle + " · mailpilot"

	if partial == "" || partial == "list" {
		const pageSize = 50
		items, err := s.st.ListMessages(ctx, store.ListOptions{Mailbox: mailbox, View: view, Limit: pageSize + 1, BeforeID: before})
		if err != nil {
			s.fail(w, "list", err)
			return
		}
		if len(items) > pageSize {
			items = items[:pageSize]
			page.MoreHref = "/v/" + view + "?avant=" + itoa(items[len(items)-1].ID)
		}
		val, unread := s.unreadSet(ctx, mailbox)
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
		for _, v := range store.Views {
			page.Views = append(page.Views, viewLink{Name: v, Title: viewTitles[v], Icon: viewIcons[v], Count: counts[v],
				Active: v == view, AI: v != store.ViewInbox})
		}
	}
	if selected != 0 && (partial == "" || partial == "pane") {
		pane, err := s.pane(r, selected, view)
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

func (s *Server) pane(r *http.Request, id int64, view string) (*paneVM, error) {
	ctx := r.Context()
	m, err := s.st.GetMessage(ctx, id)
	if err != nil {
		return nil, err
	}
	p := &paneVM{ID: m.ID, From: addrVM{Name: m.From.Name, Email: m.From.Email}, To: addrs(m.To), Cc: addrs(m.Cc),
		Date: s.fmtFullTime(m.ReceivedAt), Subject: orNoSubject(m.Subject), Body: m.TextFull,
		BodyPurged: m.BodyPurged, HTMLOnly: m.HTMLOnly, BackHref: "/v/" + view,
		Failed: m.State == store.StateAnalysisFailed}
	if p.Body == "" {
		p.Body = m.TextClean
	}
	for _, a := range m.Attachments {
		if !a.Inline {
			p.Attachments = append(p.Attachments, a.Filename)
		}
	}
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
		return nil, err
	}
	acts, err := s.st.MessageActions(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, x := range acts {
		st := actionStatus[x.Status]
		vm := actionVM{Text: x.Text, Status: st[0], StatusClass: st[1], Unverified: !x.QuoteVerified}
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
