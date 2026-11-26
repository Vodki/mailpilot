package mailbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Actions Louis takes from the web interface. Each one targets a single
// message by UIDVALIDITY and UID, so a renumbered mailbox can never act on
// the wrong message, and none of them deletes anything for good.

// ErrMoved means the message is no longer where it was (moved or deleted
// by another client, or the mailbox was renumbered).
var ErrMoved = errors.New("le message n'est plus à cet endroit")

// ErrSameFolder is returned when moving a message to its own folder.
var ErrSameFolder = errors.New("le message est déjà dans ce dossier")

// SetSeen sets or clears \Seen on one message.
func (o *Ops) SetSeen(ctx context.Context, at Stored, seen bool) error {
	return o.session(ctx, func(c *imapclient.Client) error {
		if err := selectAt(c, at); err != nil {
			return err
		}
		op := imap.StoreFlagsAdd
		if !seen {
			op = imap.StoreFlagsDel
		}
		return c.Store(imap.UIDSetNum(imap.UID(at.UID)), &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close()
	})
}

// Target names a destination: a folder, or a role ("archive", "trash")
// resolved from the configuration or SPECIAL-USE.
type Target struct {
	Folder string
	Role   string
}

// Fallback names, only used when the server announces no special use and
// nothing is configured. A folder must already exist under one of them.
var fallbackNames = map[string][]string{
	"archive": {"Archive", "Archives", "INBOX.Archive", "INBOX.Archives"},
	"trash":   {"Trash", "Corbeille", "Éléments supprimés", "Deleted Items", "Deleted Messages", "INBOX.Trash"},
	// Display only (the folder menu): never a move target by role.
	"sent":   {"Sent", "Envoyés", "Éléments envoyés", "Sent Items", "Sent Messages", "INBOX.Sent"},
	"drafts": {"Drafts", "Brouillons", "INBOX.Drafts"},
	"junk":   {"Junk", "Spam", "Courrier indésirable", "Junk E-mail", "INBOX.Junk"},
}

// Move moves one message to a folder and returns where it now is (UID 0
// when the server does not say). Without MOVE it copies, then flags the
// original \Deleted and expunges it by UID only (UIDPLUS): a plain EXPUNGE
// could remove other messages Louis flagged elsewhere, so without UIDPLUS
// the original is left flagged.
func (o *Ops) Move(ctx context.Context, at Stored, to Target) (*Stored, error) {
	var res Stored
	err := o.session(ctx, func(c *imapclient.Client) error {
		dest, err := o.resolve(c, to)
		if err != nil {
			return err
		}
		if dest == at.Folder {
			return ErrSameFolder
		}
		res.Folder = dest
		if err := selectAt(c, at); err != nil {
			return err
		}
		set := imap.UIDSetNum(imap.UID(at.UID))
		if c.Caps().Has(imap.CapMove) || c.Caps().Has(imap.CapIMAP4rev2) {
			data, err := c.Move(set, dest).Wait()
			if err != nil {
				return fmt.Errorf("move: %w", err)
			}
			res.UIDValidity, res.UID = data.UIDValidity, firstUID(data.DestUIDs)
			return nil
		}
		data, err := c.Copy(set, dest).Wait()
		if err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		res.UIDValidity, res.UID = data.UIDValidity, firstUID(data.DestUIDs)
		if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if c.Caps().Has(imap.CapUIDPlus) {
			if err := c.UIDExpunge(set).Close(); err != nil {
				return fmt.Errorf("uid expunge: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Resolve returns the folder a target designates.
func (o *Ops) Resolve(ctx context.Context, to Target) (string, error) {
	var name string
	err := o.session(ctx, func(c *imapclient.Client) error {
		var err error
		name, err = o.resolve(c, to)
		return err
	})
	return name, err
}

func (o *Ops) resolve(c *imapclient.Client, to Target) (string, error) {
	list, err := o.list(c)
	if err != nil {
		return "", err
	}
	exists := func(name string) bool {
		for _, mb := range list {
			if mb.Mailbox == name {
				for _, a := range mb.Attrs {
					if a == imap.MailboxAttrNoSelect || a == imap.MailboxAttrNonExistent {
						return false
					}
				}
				return true
			}
		}
		return false
	}
	if to.Role == "" {
		if !exists(to.Folder) {
			return "", fmt.Errorf("dossier %q introuvable", to.Folder)
		}
		return to.Folder, nil
	}
	var configured string
	var attr imap.MailboxAttr
	switch to.Role {
	case "archive":
		configured, attr = o.cfg.ArchiveFolder, imap.MailboxAttrArchive
	case "trash":
		configured, attr = o.cfg.TrashFolder, imap.MailboxAttrTrash
	default:
		return "", fmt.Errorf("rôle de dossier inconnu %q", to.Role)
	}
	if configured != "" {
		if !exists(configured) {
			return "", fmt.Errorf("dossier %q introuvable", configured)
		}
		return configured, nil
	}
	for _, mb := range list {
		for _, a := range mb.Attrs {
			if a == attr {
				return mb.Mailbox, nil
			}
		}
	}
	for _, n := range fallbackNames[to.Role] {
		if exists(n) {
			return n, nil
		}
	}
	return "", fmt.Errorf("aucun dossier %s : renseigne imap.%s_folder", to.Role, to.Role)
}

func selectAt(c *imapclient.Client, at Stored) error {
	sel, err := c.Select(at.Folder, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %s: %w", at.Folder, err)
	}
	if sel.UIDValidity != at.UIDValidity || at.UID == 0 {
		return ErrMoved
	}
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(imap.UID(at.UID))}}, nil).Wait()
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	if len(found.AllUIDs()) != 1 {
		return ErrMoved
	}
	return nil
}

func firstUID(set imap.NumSet) uint32 {
	if s, ok := set.(imap.UIDSet); ok {
		if uids, ok := s.Nums(); ok && len(uids) > 0 {
			return uint32(uids[0])
		}
	}
	return 0
}
