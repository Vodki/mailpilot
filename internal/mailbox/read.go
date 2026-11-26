package mailbox

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Unseen lists the UIDs without \Seen in a mailbox. The mailbox is opened
// with EXAMINE (read-only) and SEARCH changes no flag.
func (o *Ops) Unseen(ctx context.Context, mailbox string) (uidValidity uint32, uids []uint32, err error) {
	err = o.session(ctx, func(c *imapclient.Client) error {
		sel, err := c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return fmt.Errorf("examine %s: %w", mailbox, err)
		}
		uidValidity = sel.UIDValidity
		data, err := c.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
		if err != nil {
			return fmt.Errorf("search unseen: %w", err)
		}
		for _, u := range data.AllUIDs() {
			uids = append(uids, uint32(u))
		}
		return nil
	})
	return uidValidity, uids, err
}
