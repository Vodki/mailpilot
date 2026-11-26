package mailbox

import (
	"context"
	"fmt"
	"sort"

	"github.com/emersion/go-imap/v2"

	"github.com/Vodki/mailpilot/internal/config"
)

// ProbeResult is what `mailpilot check` reports about the IMAP server.
type ProbeResult struct {
	CapsBeforeLogin []string
	CapsAfterLogin  []string
	HasIdle         bool
	HasSpecialUse   bool
	// SpecialUse maps an attribute such as \Drafts or \Sent to a mailbox name.
	SpecialUse  map[string]string
	Mailbox     string
	UIDValidity uint32
	UIDNext     uint32
	NumMessages uint32
}

// Probe connects, logs in, lists capabilities and special-use folders, and
// opens the watched mailbox read-only (EXAMINE) so no flag can change.
func Probe(ctx context.Context, cfg config.IMAPConfig) (*ProbeResult, error) {
	c, err := Dial(ctx, cfg, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	res := &ProbeResult{Mailbox: cfg.Mailbox, SpecialUse: map[string]string{}}
	res.CapsBeforeLogin = capNames(c.Caps())

	if err := c.Login(cfg.Username, cfg.Password.Reveal()).Wait(); err != nil {
		return res, fmt.Errorf("login: %w", err)
	}
	// Some servers only announce the full set after login; ask again.
	caps, err := c.Capability().Wait()
	if err != nil {
		return res, fmt.Errorf("capability: %w", err)
	}
	res.CapsAfterLogin = capNames(caps)
	res.HasIdle = caps.Has(imap.CapIdle)
	res.HasSpecialUse = caps.Has(imap.CapSpecialUse)

	if res.HasSpecialUse {
		list, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
		if err != nil {
			return res, fmt.Errorf("list: %w", err)
		}
		for _, mb := range list {
			for _, a := range mb.Attrs {
				switch a {
				case imap.MailboxAttrDrafts, imap.MailboxAttrSent, imap.MailboxAttrTrash,
					imap.MailboxAttrJunk, imap.MailboxAttrArchive, imap.MailboxAttrAll, imap.MailboxAttrFlagged:
					res.SpecialUse[string(a)] = mb.Mailbox
				}
			}
		}
	}

	sel, err := c.Select(cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return res, fmt.Errorf("examine %s: %w", cfg.Mailbox, err)
	}
	res.UIDValidity = sel.UIDValidity
	res.UIDNext = uint32(sel.UIDNext)
	res.NumMessages = sel.NumMessages

	if err := c.Logout().Wait(); err != nil {
		return res, fmt.Errorf("logout: %w", err)
	}
	return res, nil
}

func capNames(set imap.CapSet) []string {
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out
}
