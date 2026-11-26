package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Vodki/mailpilot/internal/parse"
	"github.com/Vodki/mailpilot/internal/store"
)

// folderPage shows a folder mailpilot does not watch (archives, trash,
// sent, Louis's own folders), read live from the server with EXAMINE.
// Mails the store knows keep their AI summary.
func (s *Server) folderPage(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	ctx := r.Context()
	q := r.URL.Query()
	name := q.Get("f")
	if name == s.cfg.IMAP.Mailbox {
		http.Redirect(w, r, "/v/inbox", http.StatusSeeOther)
		return
	}
	role, ok := s.folderRole(ctx, name)
	if !ok || s.mb == nil {
		http.NotFound(w, r)
		return
	}
	before, _ := strconv.ParseUint(q.Get("avant"), 10, 32)
	selUID, _ := strconv.ParseUint(q.Get("u"), 10, 32)
	selVal, _ := strconv.ParseUint(q.Get("v"), 10, 32)
	partial := r.Header.Get("X-Partial")
	base := "/dossier?f=" + url.QueryEscape(name)
	title := folderTitle(Folder{Name: name, Role: role})

	page := pageVM{ViewTitle: title, Folder: name, CSRF: ws.CSRF, Account: s.cfg.IMAP.Username,
		Version: s.version, SendEnabled: s.cfg.SMTP.SendEnabled, CanCompose: s.comp != nil}
	page.Title = title + " · mailpilot"

	if partial == "" || partial == "list" {
		const pageSize = 50
		val, envs, err := s.mb.List(ctx, name, uint32(before), pageSize+1)
		if err != nil {
			s.fail(w, "folder list", err)
			return
		}
		if len(envs) > pageSize {
			envs = envs[:pageSize]
			page.MoreHref = base + "&avant=" + strconv.FormatUint(uint64(envs[len(envs)-1].UID), 10)
		}
		ids := make([]string, 0, len(envs))
		for _, e := range envs {
			ids = append(ids, e.MessageID)
		}
		known, err := s.st.KnownByMessageID(ctx, ids)
		if err != nil {
			s.fail(w, "known", err)
			return
		}
		for _, e := range envs {
			from := e.FromName
			if strings.TrimSpace(from) == "" {
				from = e.FromEmail
			}
			if from == "" {
				from = "(expéditeur inconnu)"
			}
			vm := itemVM{ID: int64(e.UID), FromName: from, Subject: orNoSubject(e.Subject), Time: s.fmtListTime(e.Date),
				Unread: !e.Seen, Selected: uint32(selUID) == e.UID,
				Href: base + "&v=" + strconv.FormatUint(uint64(val), 10) + "&u=" + strconv.FormatUint(uint64(e.UID), 10)}
			if k, ok := known[e.MessageID]; ok {
				if k.Analyzed {
					vm.Prio, vm.PrioLabel, vm.CatLabel, vm.Summary = k.Priority, prioLabels[k.Priority], catLabels[k.Category], k.Summary
				}
				vm.Suspect = k.Suspect
				// Moved without the server giving the new UID: now we know it.
				if k.UID == 0 && k.Mailbox == name {
					_ = s.st.Relocate(ctx, k.ID, name, val, e.UID)
				}
			}
			page.Items = append(page.Items, vm)
		}
		counts, err := s.st.ViewCounts(ctx, 1, s.cfg.IMAP.Mailbox)
		if err != nil {
			s.fail(w, "counts", err)
			return
		}
		page.Views = viewLinks(counts, "")
		page.Folders = s.folderLinks(r, name)
	}
	if selUID != 0 && (partial == "" || partial == "pane") {
		page.Pane = s.serverPane(r, Loc{Folder: name, UIDValidity: uint32(selVal), UID: uint32(selUID)}, base, ws)
		page.Title = page.Pane.Subject + " · mailpilot"
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

// serverPane reads a mail from the server (BODY.PEEK: it stays unread) and
// adds what the store knows about it.
func (s *Server) serverPane(r *http.Request, at Loc, listHref string, ws *store.WebSession) *paneVM {
	ctx := r.Context()
	p := &paneVM{Loc: at, BackHref: listHref, ListHref: listHref, FromServer: true, ByLoc: true}
	pm, err := s.readFromServer(ctx, at)
	if err != nil {
		if !errors.Is(err, ErrMoved) {
			s.log.Warn("read from server", "err", err)
		}
		p.Missing, p.Subject = true, "Mail introuvable"
		s.fillActions(r, p, ws)
		return p
	}
	p.From = addrVM{Name: pm.From.Name, Email: pm.From.Email}
	p.To, p.Cc = addrsParsed(pm.To), addrsParsed(pm.Cc)
	p.Subject, p.Body, p.HTMLOnly = orNoSubject(pm.Subject), pm.TextFull, pm.HTMLOnly
	if !pm.Date.IsZero() {
		p.Date = s.fmtFullTime(pm.Date)
	}
	for _, a := range pm.Attachments {
		if !a.Inline {
			p.Attachments = append(p.Attachments, a.Filename)
		}
	}
	if _, envs, err := s.mb.List(ctx, at.Folder, at.UID+1, 1); err == nil && len(envs) == 1 && envs[0].UID == at.UID {
		p.Unread = !envs[0].Seen
	}
	if id, err := s.st.IDAt(ctx, at.Folder, at.UIDValidity, at.UID); err == nil {
		p.ID = id
	} else if k, err := s.st.KnownByMessageID(ctx, []string{pm.MessageID}); err == nil {
		if kk, ok := k[pm.MessageID]; ok {
			p.ID = kk.ID
		}
	}
	if p.ID != 0 {
		if err := s.fillAI(ctx, p, p.ID); err != nil {
			s.log.Warn("pane analysis", "err", err)
		}
	}
	s.fillActions(r, p, ws)
	return p
}

func addrsParsed(in []parse.Address) []addrVM {
	out := make([]addrVM, 0, len(in))
	for _, a := range in {
		out = append(out, addrVM{Name: a.Name, Email: a.Email})
	}
	return out
}
