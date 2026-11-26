package main

import (
	"context"
	"errors"

	"github.com/Vodki/mailpilot/internal/mailbox"
	"github.com/Vodki/mailpilot/internal/web"
)

// webMailbox adapts mailbox.Ops to what the web interface may do, so the
// web package never imports mailbox (archtest).
type webMailbox struct{ ops *mailbox.Ops }

func mapErr(err error) error {
	switch {
	case errors.Is(err, mailbox.ErrMoved):
		return web.ErrMoved
	case errors.Is(err, mailbox.ErrSameFolder):
		return web.ErrSameFolder
	}
	return err
}

func stored(at web.Loc) mailbox.Stored {
	return mailbox.Stored{Folder: at.Folder, UIDValidity: at.UIDValidity, UID: at.UID}
}

func (m webMailbox) Status(ctx context.Context, folder string) (*web.MailboxStatus, error) {
	st, err := m.ops.Status(ctx, folder)
	if err != nil {
		return nil, err
	}
	return &web.MailboxStatus{UIDValidity: st.UIDValidity, UIDNext: st.UIDNext, Present: st.Present, Unseen: st.Unseen}, nil
}

func (m webMailbox) Folders(ctx context.Context) ([]web.Folder, error) {
	list, err := m.ops.Folders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]web.Folder, 0, len(list))
	for _, f := range list {
		out = append(out, web.Folder{Name: f.Name, Role: f.Role})
	}
	return out, nil
}

func (m webMailbox) List(ctx context.Context, folder string, before uint32, limit int) (uint32, []web.Envelope, error) {
	val, envs, err := m.ops.List(ctx, folder, before, limit)
	if err != nil {
		return 0, nil, err
	}
	out := make([]web.Envelope, 0, len(envs))
	for _, e := range envs {
		out = append(out, web.Envelope{UID: e.UID, MessageID: e.MessageID, FromName: e.FromName, FromEmail: e.FromEmail,
			Subject: e.Subject, Date: e.Date, Seen: e.Seen, Size: e.Size})
	}
	return val, out, nil
}

func (m webMailbox) Fetch(ctx context.Context, at web.Loc) ([]byte, error) {
	raw, err := m.ops.Fetch(ctx, at.Folder, at.UIDValidity, at.UID)
	return raw, mapErr(err)
}

func (m webMailbox) SetSeen(ctx context.Context, at web.Loc, seen bool) error {
	return mapErr(m.ops.SetSeen(ctx, stored(at), seen))
}

func (m webMailbox) Move(ctx context.Context, at web.Loc, to web.Target) (*web.Loc, error) {
	res, err := m.ops.Move(ctx, stored(at), mailbox.Target{Folder: to.Folder, Role: to.Role})
	if err != nil {
		return nil, mapErr(err)
	}
	return &web.Loc{Folder: res.Folder, UIDValidity: res.UIDValidity, UID: res.UID}, nil
}
