package mailbox

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Read-only operations: every mailbox is opened with EXAMINE and bodies are
// fetched with BODY.PEEK, so nothing here changes a flag.

// Status is the state of a mailbox the web interface needs: which UIDs are
// still there, and which are unread.
type Status struct {
	UIDValidity uint32
	UIDNext     uint32
	Present     []uint32
	Unseen      []uint32
}

// Status reads the UIDs present and unread in a mailbox.
func (o *Ops) Status(ctx context.Context, mailbox string) (*Status, error) {
	var st Status
	err := o.session(ctx, func(c *imapclient.Client) error {
		sel, err := c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return fmt.Errorf("examine %s: %w", mailbox, err)
		}
		st.UIDValidity, st.UIDNext = sel.UIDValidity, uint32(sel.UIDNext)
		if sel.NumMessages == 0 {
			return nil
		}
		all, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			return fmt.Errorf("search all: %w", err)
		}
		unseen, err := c.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
		if err != nil {
			return fmt.Errorf("search unseen: %w", err)
		}
		for _, u := range all.AllUIDs() {
			st.Present = append(st.Present, uint32(u))
		}
		for _, u := range unseen.AllUIDs() {
			st.Unseen = append(st.Unseen, uint32(u))
		}
		return nil
	})
	return &st, err
}

// Folder is one mailbox of the account.
type Folder struct {
	Name string
	Role string // inbox, archive, trash, sent, drafts, junk or ""
}

var roleAttrs = map[imap.MailboxAttr]string{
	imap.MailboxAttrArchive: "archive", imap.MailboxAttrTrash: "trash", imap.MailboxAttrSent: "sent",
	imap.MailboxAttrDrafts: "drafts", imap.MailboxAttrJunk: "junk",
}

// Folders lists the selectable mailboxes, with their special use when the
// server announces it (or the folder is configured).
func (o *Ops) Folders(ctx context.Context) ([]Folder, error) {
	var out []Folder
	err := o.session(ctx, func(c *imapclient.Client) error {
		list, err := o.list(c)
		if err != nil {
			return err
		}
		configured := map[string]string{
			o.cfg.ArchiveFolder: "archive", o.cfg.TrashFolder: "trash",
			o.cfg.SentFolder: "sent", o.cfg.DraftsFolder: "drafts",
		}
		for _, mb := range list {
			if slices.Contains(mb.Attrs, imap.MailboxAttrNoSelect) || slices.Contains(mb.Attrs, imap.MailboxAttrNonExistent) {
				continue
			}
			f := Folder{Name: mb.Mailbox}
			if r, ok := configured[mb.Mailbox]; ok && mb.Mailbox != "" {
				f.Role = r
			}
			for _, a := range mb.Attrs {
				if r, ok := roleAttrs[a]; ok && f.Role == "" {
					f.Role = r
				}
			}
			if f.Role == "" && (mb.Mailbox == "INBOX" || mb.Mailbox == o.cfg.Mailbox) {
				f.Role = "inbox"
			}
			out = append(out, f)
		}
		// No special use announced: recognise the usual names.
		for _, role := range []string{"archive", "trash", "sent", "drafts", "junk"} {
			if slices.ContainsFunc(out, func(f Folder) bool { return f.Role == role }) {
				continue
			}
			for _, n := range fallbackNames[role] {
				if i := slices.IndexFunc(out, func(f Folder) bool { return f.Name == n && f.Role == "" }); i >= 0 {
					out[i].Role = role
					break
				}
			}
		}
		return nil
	})
	return out, err
}

func (o *Ops) list(c *imapclient.Client) ([]*imap.ListData, error) {
	var opts *imap.ListOptions
	if c.Caps().Has(imap.CapSpecialUse) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	list, err := c.List("", "*", opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	return list, nil
}

// Envelope is a message header line read from IMAP, for the folders
// mailpilot does not watch.
type Envelope struct {
	UID       uint32
	MessageID string
	FromName  string
	FromEmail string
	Subject   string
	Date      time.Time
	Seen      bool
	Size      int64
}

// List returns the envelopes of the newest messages of a folder (UID below
// before, 0 for the newest), newest first.
func (o *Ops) List(ctx context.Context, folder string, before uint32, limit int) (uidValidity uint32, out []Envelope, err error) {
	err = o.session(ctx, func(c *imapclient.Client) error {
		sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return fmt.Errorf("examine %s: %w", folder, err)
		}
		uidValidity = sel.UIDValidity
		if sel.NumMessages == 0 {
			return nil
		}
		all, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
		uids := all.AllUIDs()
		slices.Sort(uids)
		if before > 0 {
			i, _ := slices.BinarySearch(uids, imap.UID(before))
			uids = uids[:i]
		}
		if len(uids) > limit {
			uids = uids[len(uids)-limit:]
		}
		if len(uids) == 0 {
			return nil
		}
		msgs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
			UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true,
		}).Collect()
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		for _, m := range msgs {
			e := Envelope{UID: uint32(m.UID), Seen: slices.Contains(m.Flags, imap.FlagSeen), Size: m.RFC822Size, Date: m.InternalDate}
			if env := m.Envelope; env != nil {
				e.MessageID, e.Subject = env.MessageID, env.Subject
				if !env.Date.IsZero() {
					e.Date = env.Date
				}
				if len(env.From) > 0 {
					e.FromName, e.FromEmail = env.From[0].Name, env.From[0].Addr()
				}
			}
			out = append(out, e)
		}
		slices.SortFunc(out, func(a, b Envelope) int { return int(b.UID) - int(a.UID) })
		return nil
	})
	return uidValidity, out, err
}

// MaxFetchSize bounds the raw message Fetch reads.
const MaxFetchSize = 25 << 20

// Fetch reads one raw message without setting \Seen (BODY.PEEK[]), for a
// folder mailpilot does not watch or a body purged from the store.
func (o *Ops) Fetch(ctx context.Context, folder string, uidValidity, uid uint32) ([]byte, error) {
	var raw []byte
	err := o.session(ctx, func(c *imapclient.Client) error {
		sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return fmt.Errorf("examine %s: %w", folder, err)
		}
		if sel.UIDValidity != uidValidity {
			return ErrMoved
		}
		section := &imap.FetchItemBodySection{Peek: true}
		msgs, err := c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		if len(msgs) != 1 {
			return ErrMoved
		}
		if msgs[0].RFC822Size > MaxFetchSize {
			return fmt.Errorf("message trop gros (%d octets)", msgs[0].RFC822Size)
		}
		raw = msgs[0].FindBodySection(section)
		return nil
	})
	return raw, err
}
