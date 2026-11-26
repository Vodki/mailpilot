package mailbox

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestOpsDraftLifecycleAndMarkSeen(t *testing.T) {
	addr := startServer(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}})
	cfg := cfgFor(t, addr, "secret")
	ctx := context.Background()
	ops := NewOps(cfg)

	if _, err := ops.AppendDraft(ctx, []byte("x")); !errors.Is(err, ErrNoFolder) {
		t.Fatalf("without SPECIAL-USE nor config, drafts must be refused: %v", err)
	}
	cfg.DraftsFolder, cfg.SentFolder = "INBOX", "INBOX"
	ops = NewOps(cfg)
	raw := []byte("From: louis@example.test\r\nTo: p@x.test\r\nSubject: RE: x\r\nMessage-ID: <d1@example.test>\r\n\r\nBody\r\n")
	st, err := ops.AppendDraft(ctx, raw)
	if err != nil || st.UID == 0 || st.Folder != "INBOX" {
		t.Fatalf("append: %+v %v", st, err)
	}
	ok, err := ops.HasMessage(ctx, "INBOX", "<d1@example.test>")
	if err != nil || !ok {
		t.Fatalf("draft not found: %v", err)
	}
	if err := ops.Delete(ctx, *st, "d1@example.test"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ops.HasMessage(ctx, "INBOX", "<d1@example.test>"); ok {
		t.Fatal("draft still present after delete")
	}

	st2, _ := ops.AppendSent(ctx, []byte("Subject: y\r\nMessage-ID: <s1@example.test>\r\n\r\nz\r\n"))
	// The sent copy is \Seen; append an unread one and mark it.
	st3, _ := ops.appendTo(ctx, "INBOX", "", []byte("Subject: z\r\nMessage-ID: <u1@example.test>\r\n\r\nz\r\n"), nil)
	if err := ops.MarkSeen(ctx, "INBOX", st3.UIDValidity+1, st3.UID); err == nil {
		t.Fatal("wrong UIDVALIDITY must be refused")
	}
	if err := ops.MarkSeen(ctx, "INBOX", st3.UIDValidity, st3.UID); err != nil {
		t.Fatal(err)
	}
	c, _ := Dial(ctx, cfg, nil)
	defer func() { _ = c.Close() }()
	_ = c.Login(cfg.Username, "secret").Wait()
	_, _ = c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	msgs, _ := c.Fetch(imap.UIDSetNum(imap.UID(st2.UID), imap.UID(st3.UID)), &imap.FetchOptions{UID: true, Flags: true}).Collect()
	for _, m := range msgs {
		if !slices.Contains(m.Flags, imap.FlagSeen) {
			t.Fatalf("uid %d not seen: %v", m.UID, m.Flags)
		}
	}
}
