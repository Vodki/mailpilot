package web

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// fakeMailbox is an in-memory account. writes counts the calls that change
// it, so tests can prove browsing changes nothing.
type fakeMailbox struct {
	mu      sync.Mutex
	folders map[string]*fakeFolder
	roles   map[string]string
	writes  int
}

type fakeFolder struct {
	val  uint32
	next uint32
	msgs map[uint32]*fakeMsg
}

type fakeMsg struct {
	id, from, subject, body string
	seen                    bool
}

func newFakeMailbox() *fakeMailbox {
	f := &fakeMailbox{folders: map[string]*fakeFolder{}, roles: map[string]string{"INBOX": "inbox", "Archive": "archive", "Trash": "trash"}}
	for i, n := range []string{"INBOX", "Archive", "Trash", "Factures"} {
		f.folders[n] = &fakeFolder{val: uint32(7 + i), next: 1, msgs: map[uint32]*fakeMsg{}}
	}
	return f
}

func (f *fakeMailbox) add(folder string, m *fakeMsg) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	fo := f.folders[folder]
	uid := fo.next
	fo.next++
	fo.msgs[uid] = m
	return uid
}

func (f *fakeMailbox) Status(_ context.Context, folder string) (*MailboxStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fo := f.folders[folder]
	st := &MailboxStatus{UIDValidity: fo.val, UIDNext: fo.next}
	for uid, m := range fo.msgs {
		st.Present = append(st.Present, uid)
		if !m.seen {
			st.Unseen = append(st.Unseen, uid)
		}
	}
	slices.Sort(st.Present)
	return st, nil
}

func (f *fakeMailbox) Folders(context.Context) ([]Folder, error) {
	var out []Folder
	for n := range f.folders {
		out = append(out, Folder{Name: n, Role: f.roles[n]})
	}
	return out, nil
}

func (f *fakeMailbox) List(_ context.Context, folder string, before uint32, limit int) (uint32, []Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fo := f.folders[folder]
	var out []Envelope
	for uid, m := range fo.msgs {
		if before == 0 || uid < before {
			out = append(out, Envelope{UID: uid, MessageID: m.id, FromEmail: m.from, Subject: m.subject, Seen: m.seen,
				Date: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)})
		}
	}
	slices.SortFunc(out, func(a, b Envelope) int { return int(b.UID) - int(a.UID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return fo.val, out, nil
}

func (f *fakeMailbox) get(at Loc) (*fakeFolder, *fakeMsg, error) {
	fo, ok := f.folders[at.Folder]
	if !ok || fo.val != at.UIDValidity {
		return nil, nil, ErrMoved
	}
	m, ok := fo.msgs[at.UID]
	if !ok {
		return nil, nil, ErrMoved
	}
	return fo, m, nil
}

func (f *fakeMailbox) Fetch(_ context.Context, at Loc) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, m, err := f.get(at)
	if err != nil {
		return nil, err
	}
	return []byte("From: " + m.from + "\r\nTo: louis@example.test\r\nSubject: " + m.subject + "\r\nMessage-ID: <" + strings.Trim(m.id, "<>") +
		">\r\nDate: Fri, 20 Nov 2026 11:00:00 +0100\r\n\r\n" + m.body + "\r\n"), nil
}

func (f *fakeMailbox) SetSeen(_ context.Context, at Loc, seen bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, m, err := f.get(at)
	if err != nil {
		return err
	}
	f.writes++
	m.seen = seen
	return nil
}

func (f *fakeMailbox) Move(_ context.Context, at Loc, to Target) (*Loc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dest := to.Folder
	if to.Role != "" {
		for n, r := range f.roles {
			if r == to.Role {
				dest = n
			}
		}
	}
	if dest == at.Folder {
		return nil, ErrSameFolder
	}
	fo, m, err := f.get(at)
	if err != nil {
		return nil, err
	}
	df := f.folders[dest]
	f.writes++
	delete(fo.msgs, at.UID)
	uid := df.next
	df.next++
	df.msgs[uid] = m
	return &Loc{Folder: dest, UIDValidity: df.val, UID: uid}, nil
}
