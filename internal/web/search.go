package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Vodki/mailpilot/internal/store"
)

// Assistant answers a question from Louis's own mails (« Demander à ta
// boîte »). The production implementation is the /ask flow of the app:
// full-text search first, then the LLM answers from those mails only, and
// every citation is checked against them.
type Assistant interface {
	Ask(ctx context.Context, question string) (*Answer, error)
}

// Answer is the reply to a question. Sources are the cited stored
// messages, in citation order ([1] is Sources[0]); Found is how many mails
// the search gave the LLM (0: no LLM call, nothing matched).
type Answer struct {
	Text    string
	Sources []int64
	Found   int
}

const maxQuery = 500

type searchVM struct {
	Q       string
	Filters []string // what was understood, for the list head
	Count   int
	Empty   bool // nothing to search for
	CanAsk  bool
}

type sourceVM struct {
	N                   int
	Href                string
	From, Date, Subject string
	Summary             string
}

type answerVM struct {
	Q        string
	Text     string
	Sources  []sourceVM
	Found    int
	Error    string
	BackHref string
}

// parseSearch reads the search box: free words, plus « de:pierre »,
// « avant:15/11/2026 » and « après:2026-11-01 » (also « apres: »).
func parseSearch(q string, loc *time.Location, now time.Time) (store.SearchOptions, []string) {
	var o store.SearchOptions
	var words, filters []string
	for _, f := range strings.Fields(q) {
		key, val, ok := strings.Cut(f, ":")
		if !ok || val == "" {
			words = append(words, f)
			continue
		}
		switch strings.ToLower(key) {
		case "de", "from":
			o.From = strings.TrimSpace(o.From + " " + val)
			filters = append(filters, "de "+val)
		case "avant", "before":
			if d, ok := parseDay(val, loc, now); ok {
				o.Before = d
				filters = append(filters, "avant le "+d.Format("02/01/2006"))
				continue
			}
			words = append(words, f)
		case "après", "apres", "after", "depuis":
			if d, ok := parseDay(val, loc, now); ok {
				o.After = d
				filters = append(filters, "depuis le "+d.Format("02/01/2006"))
				continue
			}
			words = append(words, f)
		default:
			words = append(words, f)
		}
	}
	o.Words = strings.Join(words, " ")
	return o, filters
}

// parseDay reads 2026-11-15, 15/11/2026 or 15/11 (this year), as the start
// of that day in Louis's time zone.
func parseDay(v string, loc *time.Location, now time.Time) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "02/01/2006", "2/1/2006"} {
		if d, err := time.ParseInLocation(layout, v, loc); err == nil {
			return d, true
		}
	}
	for _, layout := range []string{"02/01", "2/1"} {
		if d, err := time.ParseInLocation(layout, v, loc); err == nil {
			return time.Date(now.In(loc).Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc), true
		}
	}
	return time.Time{}, false
}

func searchHref(q string) string { return "/recherche?q=" + url.QueryEscape(q) }

func (s *Server) loc() *time.Location {
	if s.cfg.Location != nil {
		return s.cfg.Location
	}
	return time.UTC
}

// searchPage: GET /recherche?q=…[&m=id]. The results in the list, the
// selected mail (or the question box) in the pane.
func (s *Server) searchPage(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > maxQuery {
		q = truncate(q, maxQuery)
	}
	selected, _ := strconv.ParseInt(r.URL.Query().Get("m"), 10, 64)
	partial := r.Header.Get("X-Partial")
	page, ok := s.searchList(w, r, ws, q, partial == "" || partial == "list")
	if !ok {
		return
	}
	if selected != 0 && (partial == "" || partial == "pane") {
		p, err := s.pane(r, selected, store.ViewInbox, ws)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.fail(w, "pane", err)
			return
		}
		back := searchHref(q)
		p.BackHref, p.ListHref, p.SelfHref = back, back, back+"&m="+itoa(p.ID)
		for i := range p.Thread {
			p.Thread[i].Href = back + "&m=" + itoa(p.Thread[i].ID)
		}
		page.Pane = p
		page.Title = p.Subject + " · mailpilot"
		for i := range page.Items {
			page.Items[i].Selected = page.Items[i].ID == selected
		}
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

// searchList builds the page around the results of q.
func (s *Server) searchList(w http.ResponseWriter, r *http.Request, ws *store.WebSession, q string, withList bool) (pageVM, bool) {
	ctx := r.Context()
	page := pageVM{ViewTitle: "Recherche", CSRF: ws.CSRF, Account: s.cfg.IMAP.Username, Version: s.version,
		SendEnabled: s.cfg.SMTP.SendEnabled, CanCompose: s.comp != nil, Query: q}
	page.Title = "Recherche · mailpilot"
	sv := &searchVM{Q: q, CanAsk: s.ask != nil && q != ""}
	page.Search = sv
	if !withList {
		return page, true
	}
	val, unread := s.inboxStatus(ctx)
	opts, filters := parseSearch(q, s.loc(), s.now())
	sv.Filters = filters
	sv.Empty = opts.Empty()
	opts.Limit = 100
	items, err := s.st.SearchMessages(ctx, opts)
	if err != nil {
		s.fail(w, "search", err)
		return page, false
	}
	sv.Count = len(items)
	back := searchHref(q)
	for _, it := range items {
		vm := s.item(it, back+"&m="+itoa(it.ID), val, unread)
		if it.Absent {
			vm.Where = "Déplacé ailleurs"
		} else if it.Mailbox != s.cfg.IMAP.Mailbox {
			vm.Where = s.folderDisplay(r, it.Mailbox)
		}
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

// item turns a stored row into a list entry.
func (s *Server) item(it store.ListItem, href string, val uint32, unread map[uint32]bool) itemVM {
	vm := itemVM{ID: it.ID, Href: href, FromName: displayName(it.From), Subject: orNoSubject(it.Subject),
		Time: s.fmtListTime(it.ReceivedAt), Attach: it.HasAttachments, Draft: it.HasDraft,
		Unread:       it.Mailbox == s.cfg.IMAP.Mailbox && unread != nil && it.UIDValidity == val && unread[it.UID],
		TasksCreated: it.TasksCreated, TasksProposed: it.TasksProposed,
		Suspect: it.AIInstructions || it.Category == "spam_probable"}
	if it.Analyzed {
		vm.Prio, vm.PrioLabel, vm.CatLabel, vm.Summary = it.Priority, prioLabels[it.Priority], catLabels[it.Category], it.Summary
	} else {
		vm.Pending = it.State == store.StateNew
	}
	return vm
}

func (s *Server) folderDisplay(r *http.Request, name string) string {
	for _, f := range s.folders(r.Context()) {
		if f.Name == name {
			return folderTitle(f)
		}
	}
	return name
}

// askAction: POST /demander. One question, answered from the mails the
// search finds; the answer is shown in the pane with its cited mails.
func (s *Server) askAction(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	if s.ask == nil {
		http.Error(w, "assistant indisponible", http.StatusServiceUnavailable)
		return
	}
	q := strings.TrimSpace(r.PostFormValue("q"))
	if q == "" {
		http.Redirect(w, r, "/recherche", http.StatusSeeOther)
		return
	}
	if len(q) > maxQuery {
		q = truncate(q, maxQuery)
	}
	vm := &answerVM{Q: q, BackHref: searchHref(q)}
	// A slow model can take a minute or two.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	a, err := s.ask.Ask(ctx, q)
	if err != nil {
		s.log.Warn("web ask failed", "err", err)
		vm.Error = "Réponse impossible : " + err.Error()
	} else {
		vm.Text, vm.Found = a.Text, a.Found
		for i, id := range a.Sources {
			m, err := s.st.GetMessage(r.Context(), id)
			if err != nil {
				continue
			}
			when := m.SentAt
			if when.IsZero() {
				when = m.ReceivedAt
			}
			sv := sourceVM{N: i + 1, Href: vm.BackHref + "&m=" + itoa(id), From: displayName(m.From),
				Date: when.In(s.loc()).Format("02/01/2006"), Subject: orNoSubject(m.Subject)}
			if an, err := s.st.LatestAnalysis(r.Context(), id); err == nil {
				sv.Summary = an.Summary
			}
			vm.Sources = append(vm.Sources, sv)
		}
	}
	if r.Header.Get("X-Partial") == "pane" {
		s.render(w, "answer", vm)
		return
	}
	page, ok := s.searchList(w, r, ws, q, true)
	if !ok {
		return
	}
	page.Answer = vm
	page.Title = "Réponse · mailpilot"
	s.render(w, "app.html", page)
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
