package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/store"
)

// The « Aujourd'hui » page: the Telegram digest as a page. What matters
// today, what waits for an answer, the reminders to decide, and what
// arrived since midnight.

type tileVM struct {
	Label, Href, Class string
	Count              int
}

type reminderVM struct {
	ID          int64
	Text, Due   string
	Late, Today bool
	Decide      bool // proposed or failed: Louis adds or dismisses it
	Status      string
	Href        string
	From, Subj  string
}

type todayVM struct {
	Date       string
	Tiles      []tileVM
	Important  []itemVM
	Waiting    []itemVM
	Reminders  []reminderVM
	Arrived    []itemVM
	NextDigest string
	CSRF       string
}

const todayListSize = 8

func (s *Server) todayPage(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	ctx := r.Context()
	loc := s.loc()
	now := s.now().In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	mailbox := s.cfg.IMAP.Mailbox
	val, unread := s.inboxStatus(ctx)
	vm := &todayVM{Date: frDays[now.Weekday()] + " " + strconv.Itoa(now.Day()) + " " + frMonths[now.Month()],
		NextDigest: nextDigest(s.cfg.Digest.Times, now), CSRF: ws.CSRF}

	list := func(view string) ([]store.ListItem, bool) {
		items, err := s.st.ListMessages(ctx, store.ListOptions{Mailbox: mailbox, View: view, Limit: todayListSize})
		if err != nil {
			s.fail(w, "today "+view, err)
			return nil, false
		}
		return items, true
	}
	shown := map[int64]bool{}
	urgent, ok := list(store.ViewUrgent)
	if !ok {
		return
	}
	for _, it := range urgent {
		shown[it.ID] = true
		vm.Important = append(vm.Important, s.item(it, "/v/urgent?m="+itoa(it.ID), val, unread))
	}
	reply, ok := list(store.ViewReply)
	if !ok {
		return
	}
	for _, it := range reply {
		if shown[it.ID] {
			continue
		}
		shown[it.ID] = true
		vm.Waiting = append(vm.Waiting, s.item(it, "/v/reply?m="+itoa(it.ID), val, unread))
	}
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	rems, err := s.st.PendingReminders(ctx, tomorrow, 20)
	if err != nil {
		s.fail(w, "today reminders", err)
		return
	}
	todayStr := now.Format("2006-01-02")
	decide := 0
	for _, x := range rems {
		rv := reminderVM{ID: x.ID, Text: x.Text, Href: "/v/todo?m=" + itoa(x.MessagePK), From: displayName(x.From),
			Subj: orNoSubject(x.Subject), Status: actionStatus[x.Status][0],
			Decide: s.comp != nil && (x.Status == store.ActionProposed || x.Status == store.ActionFailed)}
		if x.Status == store.ActionProposed || x.Status == store.ActionFailed {
			decide++
		}
		if x.DueDate != "" {
			rv.Late, rv.Today = x.DueDate < todayStr, x.DueDate == todayStr
			rv.Due = dueText(x.DueDate, x.DueTime, todayStr, tomorrow)
		}
		vm.Reminders = append(vm.Reminders, rv)
	}
	arrived, err := s.st.ReceivedSince(ctx, mailbox, midnight, 30)
	if err != nil {
		s.fail(w, "today arrived", err)
		return
	}
	for _, it := range arrived {
		if shown[it.ID] {
			continue
		}
		vm.Arrived = append(vm.Arrived, s.item(it, "/v/inbox?m="+itoa(it.ID), val, unread))
	}
	counts, err := s.st.ViewCounts(ctx, 1, mailbox)
	if err != nil {
		s.fail(w, "counts", err)
		return
	}
	unreadN := -1
	if unread != nil {
		unreadN = len(unread)
	}
	vm.Tiles = []tileVM{
		{Label: "Urgents", Href: "/v/urgent", Class: "urgent", Count: counts[store.ViewUrgent]},
		{Label: "Réponses attendues", Href: "/v/reply", Class: "reply", Count: counts[store.ViewReply]},
		{Label: "Rappels à décider", Href: "/v/todo", Class: "todo", Count: decide},
	}
	if unreadN >= 0 {
		vm.Tiles = append([]tileVM{{Label: "Non lus", Href: "/v/inbox", Count: unreadN}}, vm.Tiles...)
	}
	if r.Header.Get("X-Partial") == "pane" {
		s.render(w, "today", vm)
		return
	}
	page := pageVM{ViewTitle: "Aujourd'hui", CSRF: ws.CSRF, Account: s.cfg.IMAP.Username, Version: s.version,
		SendEnabled: s.cfg.SMTP.SendEnabled, CanCompose: s.comp != nil, Today: vm, Title: "Aujourd'hui · mailpilot",
		Views: viewLinks(counts, ""), Folders: s.folderLinks(r, "")}
	s.render(w, "app.html", page)
}

func dueText(date, hm, today, tomorrow string) string {
	var out string
	switch date {
	case today:
		out = "aujourd'hui"
	case tomorrow:
		out = "demain"
	default:
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			return ""
		}
		out = "le " + strconv.Itoa(d.Day()) + " " + frMonthsShort[d.Month()]
		if date < today {
			out = "en retard (" + out + ")"
		}
	}
	if hm != "" {
		out += " à " + strings.TrimSuffix(strings.Replace(hm, ":", "h", 1), "00")
	}
	return out
}

// nextDigest names the next Telegram digest ("18:00", "demain 08:30").
func nextDigest(times []string, now time.Time) string {
	ts := append([]string(nil), times...)
	sort.Strings(ts)
	cur := now.Format("15:04")
	for _, t := range ts {
		if t > cur {
			return t
		}
	}
	if len(ts) > 0 {
		return "demain " + ts[0]
	}
	return ""
}
