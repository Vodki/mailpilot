package mailbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/Vodki/mailpilot/internal/config"
)

// Ops performs the mutating IMAP operations, each on a short-lived
// connection. Every method is called only after an explicit action of Louis
// (or to store a draft he asked for): nothing here runs on its own.
type Ops struct {
	cfg config.IMAPConfig
}

// NewOps builds the IMAP operations helper.
func NewOps(cfg config.IMAPConfig) *Ops { return &Ops{cfg: cfg} }

// ErrNoFolder means the drafts or sent folder is unknown.
var ErrNoFolder = errors.New("dossier introuvable : le serveur n'annonce pas SPECIAL-USE, renseigne imap.drafts_folder et imap.sent_folder")

func (o *Ops) session(ctx context.Context, f func(c *imapclient.Client) error) error {
	c, err := Dial(ctx, o.cfg, nil)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := c.Login(o.cfg.Username, o.cfg.Password.Reveal()).Wait(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if err := f(c); err != nil {
		return err
	}
	return c.Logout().Wait()
}

// folder returns the configured folder, or the special-use one.
func (o *Ops) folder(c *imapclient.Client, configured string, attr imap.MailboxAttr) (string, error) {
	if configured != "" {
		return configured, nil
	}
	caps, err := c.Capability().Wait()
	if err != nil {
		return "", err
	}
	if !caps.Has(imap.CapSpecialUse) {
		return "", ErrNoFolder
	}
	list, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return "", err
	}
	for _, mb := range list {
		for _, a := range mb.Attrs {
			if a == attr {
				return mb.Mailbox, nil
			}
		}
	}
	return "", ErrNoFolder
}

// Stored locates a message appended by mailpilot.
type Stored struct {
	Folder      string
	UIDValidity uint32
	UID         uint32
}

func (o *Ops) appendTo(ctx context.Context, configured string, attr imap.MailboxAttr, raw []byte, flags []imap.Flag) (*Stored, error) {
	var st Stored
	err := o.session(ctx, func(c *imapclient.Client) error {
		folder, err := o.folder(c, configured, attr)
		if err != nil {
			return err
		}
		st.Folder = folder
		cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: time.Now()})
		if _, err := cmd.Write(raw); err != nil {
			return fmt.Errorf("append %s: %w", folder, err)
		}
		if err := cmd.Close(); err != nil {
			return fmt.Errorf("append %s: %w", folder, err)
		}
		data, err := cmd.Wait()
		if err != nil {
			return fmt.Errorf("append %s: %w", folder, err)
		}
		st.UID, st.UIDValidity = uint32(data.UID), data.UIDValidity
		return nil
	})
	return &st, err
}

// AppendDraft stores a reply draft in the drafts folder (\Draft, \Seen).
func (o *Ops) AppendDraft(ctx context.Context, raw []byte) (*Stored, error) {
	return o.appendTo(ctx, o.cfg.DraftsFolder, imap.MailboxAttrDrafts, raw, []imap.Flag{imap.FlagDraft, imap.FlagSeen})
}

// AppendSent stores a copy of a sent reply in the sent folder (\Seen).
func (o *Ops) AppendSent(ctx context.Context, raw []byte) (*Stored, error) {
	return o.appendTo(ctx, o.cfg.SentFolder, imap.MailboxAttrSent, raw, []imap.Flag{imap.FlagSeen})
}

// Delete removes a message mailpilot appended (a superseded or cancelled
// draft). When UIDVALIDITY changed or UID is unknown, or the server lacks
// UIDPLUS, the message is only flagged \Deleted: a plain EXPUNGE could
// remove other messages Louis flagged.
func (o *Ops) Delete(ctx context.Context, s Stored, messageID string) error {
	return o.session(ctx, func(c *imapclient.Client) error {
		sel, err := c.Select(s.Folder, nil).Wait()
		if err != nil {
			return fmt.Errorf("select %s: %w", s.Folder, err)
		}
		uid := imap.UID(s.UID)
		if sel.UIDValidity != s.UIDValidity || uid == 0 {
			found, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<" + messageID + ">"}}}, nil).Wait()
			if err != nil {
				return err
			}
			uids := found.AllUIDs()
			if len(uids) != 1 {
				return nil // nothing (or something ambiguous) to delete: leave it
			}
			uid = uids[0]
		}
		set := imap.UIDSetNum(uid)
		if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if c.Caps().Has(imap.CapUIDPlus) || c.Caps().Has(imap.CapIMAP4rev2) {
			if err := c.UIDExpunge(set).Close(); err != nil {
				return fmt.Errorf("uid expunge: %w", err)
			}
		}
		return nil
	})
}

// MarkSeen sets \Seen on one message of the watched mailbox (button
// "Marquer lu"). The UIDVALIDITY must match, so a renumbered mailbox can
// never mark the wrong message.
func (o *Ops) MarkSeen(ctx context.Context, mailbox string, uidValidity, uid uint32) error {
	return o.session(ctx, func(c *imapclient.Client) error {
		sel, err := c.Select(mailbox, nil).Wait()
		if err != nil {
			return fmt.Errorf("select %s: %w", mailbox, err)
		}
		if sel.UIDValidity != uidValidity {
			return errors.New("UIDVALIDITY a changé : message introuvable")
		}
		return c.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close()
	})
}

// HasMessage reports whether a folder holds a message with this Message-ID
// (tests and diagnostics).
func (o *Ops) HasMessage(ctx context.Context, folder, messageID string) (bool, error) {
	var ok bool
	err := o.session(ctx, func(c *imapclient.Client) error {
		if _, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return err
		}
		found, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: messageID}}}, nil).Wait()
		if err != nil {
			return err
		}
		ok = len(found.AllUIDs()) > 0
		return nil
	})
	return ok, err
}
