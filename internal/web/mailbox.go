package web

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Mailbox is everything the interface may do on the IMAP account. The
// production implementation (cmd/mailpilot) wraps internal/mailbox; web
// never imports it. Reads use EXAMINE and BODY.PEEK; the writes are the
// buttons Louis presses, and none of them deletes a mail for good.
type Mailbox interface {
	Status(ctx context.Context, folder string) (*MailboxStatus, error)
	Folders(ctx context.Context) ([]Folder, error)
	List(ctx context.Context, folder string, before uint32, limit int) (uidValidity uint32, envs []Envelope, err error)
	Fetch(ctx context.Context, at Loc) ([]byte, error)
	SetSeen(ctx context.Context, at Loc, seen bool) error
	Move(ctx context.Context, at Loc, to Target) (*Loc, error)
}

// Loc locates one message on the server.
type Loc struct {
	Folder      string
	UIDValidity uint32
	UID         uint32
}

// Target is a destination folder, or a role (archive, trash).
type Target struct {
	Folder string
	Role   string
}

// MailboxStatus lists the UIDs present and unread in a folder.
type MailboxStatus struct {
	UIDValidity, UIDNext uint32
	Present, Unseen      []uint32
}

// Folder is one mailbox of the account.
type Folder struct {
	Name string
	Role string // inbox, archive, trash, sent, drafts, junk or ""
}

// Envelope is one line of a folder listing read from the server.
type Envelope struct {
	UID                 uint32
	MessageID           string
	FromName, FromEmail string
	Subject             string
	Date                time.Time
	Seen                bool
	Size                int64
}

// Errors the Mailbox implementation maps its own errors to.
var (
	ErrMoved      = errors.New("ce mail n'est plus à cet endroit (déplacé ou supprimé ailleurs)")
	ErrSameFolder = errors.New("ce mail est déjà dans ce dossier")
)

// ---------- cached state ----------

// inboxStatus returns the unread UIDs of the watched mailbox, refreshed at
// most every 15 s, and hides from the lists the messages moved or deleted
// elsewhere. On error the last known state is kept (and, initially,
// nothing is shown as unread).
func (s *Server) inboxStatus(ctx context.Context) (uint32, map[uint32]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mb == nil {
		return 0, nil
	}
	if s.unseenOK && s.now().Sub(s.unseenAt) < 15*time.Second {
		return s.unseenVal, s.unseenSet
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	mailbox := s.cfg.IMAP.Mailbox
	st, err := s.mb.Status(cctx, mailbox)
	s.unseenAt = s.now()
	if err != nil {
		s.log.Warn("mailbox status unavailable", "err", err)
		return s.unseenVal, s.unseenSet
	}
	if n, err := s.st.SyncPresence(cctx, mailbox, st.UIDValidity, st.UIDNext, st.Present); err != nil {
		s.log.Warn("presence sync", "err", err)
	} else if n > 0 {
		s.log.Info("messages moved outside mailpilot", "count", n)
	}
	set := make(map[uint32]bool, len(st.Unseen))
	for _, u := range st.Unseen {
		set[u] = true
	}
	s.unseenVal, s.unseenSet, s.unseenOK = st.UIDValidity, set, true
	return st.UIDValidity, set
}

// noteSeen updates the cached unread state after an action of Louis, so
// the next list shows it without asking the server again.
func (s *Server) noteSeen(at Loc, seen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.Folder != s.cfg.IMAP.Mailbox || at.UIDValidity != s.unseenVal || s.unseenSet == nil {
		return
	}
	if seen {
		delete(s.unseenSet, at.UID)
	} else {
		s.unseenSet[at.UID] = true
	}
}

func (s *Server) invalidateStatus() {
	s.mu.Lock()
	s.unseenAt = time.Time{}
	s.mu.Unlock()
}

// folders returns the account's folders, cached for a minute. The watched
// mailbox comes first, then the special folders, then the others by name.
func (s *Server) folders(ctx context.Context) []Folder {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mb == nil {
		return nil
	}
	if s.foldersOK && s.now().Sub(s.foldersAt) < time.Minute {
		return s.folderList
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := s.mb.Folders(cctx)
	s.foldersAt = s.now()
	if err != nil {
		s.log.Warn("folders unavailable", "err", err)
		return s.folderList
	}
	rank := map[string]int{"inbox": 0, "drafts": 1, "sent": 2, "archive": 3, "junk": 4, "trash": 5, "": 6}
	slices.SortStableFunc(list, func(a, b Folder) int {
		if d := rank[a.Role] - rank[b.Role]; d != 0 {
			return d
		}
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	s.folderList, s.foldersOK = list, true
	return list
}

// folderRole tells whether a folder exists, asking the server again if it
// is not in the cache (created from another client in the last minute).
func (s *Server) folderRole(ctx context.Context, name string) (string, bool) {
	for try := 0; try < 2; try++ {
		for _, f := range s.folders(ctx) {
			if f.Name == name {
				return f.Role, true
			}
		}
		s.mu.Lock()
		stale := s.now().Sub(s.foldersAt) > 5*time.Second
		if stale {
			s.foldersOK = false
		}
		s.mu.Unlock()
		if !stale {
			break
		}
	}
	return "", false
}
