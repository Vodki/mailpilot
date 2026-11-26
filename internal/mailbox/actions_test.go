package mailbox

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/Vodki/mailpilot/internal/testutil"
)

func startFolders(t *testing.T, caps imap.CapSet, folders ...string) string {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("louis@example.test", "secret")
	for _, f := range append([]string{"INBOX"}, folders...) {
		if err := user.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", testutil.SelfSignedTLS(t))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func put(t *testing.T, ops *Ops, folder, id string) *Stored {
	t.Helper()
	raw := "From: Paul <paul@example.test>\r\nTo: louis@example.test\r\nSubject: Sujet " + id +
		"\r\nMessage-ID: <" + id + "@example.test>\r\nDate: Wed, 25 Nov 2026 10:00:00 +0100\r\n\r\nCorps " + id + "\r\n"
	st, err := ops.appendTo(context.Background(), folder, "", []byte(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStatusSeenAndPeek(t *testing.T) {
	addr := startFolders(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}, imap.CapMove: {}})
	ops := NewOps(cfgFor(t, addr, "secret"))
	ctx := context.Background()
	a, b := put(t, ops, "INBOX", "a"), put(t, ops, "INBOX", "b")

	st, err := ops.Status(ctx, "INBOX")
	if err != nil || len(st.Present) != 2 || len(st.Unseen) != 2 || st.UIDNext <= b.UID {
		t.Fatalf("status: %+v %v", st, err)
	}
	raw, err := ops.Fetch(ctx, "INBOX", a.UIDValidity, a.UID)
	if err != nil || !strings.Contains(string(raw), "Corps a") {
		t.Fatalf("fetch: %q %v", raw, err)
	}
	if st, _ := ops.Status(ctx, "INBOX"); len(st.Unseen) != 2 {
		t.Fatal("reading a body must not mark it seen")
	}
	if err := ops.SetSeen(ctx, *a, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := ops.Status(ctx, "INBOX"); !slices.Equal(st.Unseen, []uint32{b.UID}) {
		t.Fatalf("after seen: %v", st.Unseen)
	}
	if err := ops.SetSeen(ctx, *a, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := ops.Status(ctx, "INBOX"); len(st.Unseen) != 2 {
		t.Fatal("unseen not restored")
	}
	wrong := *a
	wrong.UIDValidity++
	if err := ops.SetSeen(ctx, wrong, true); !errors.Is(err, ErrMoved) {
		t.Fatalf("wrong UIDVALIDITY: %v", err)
	}
	gone := *a
	gone.UID = 999
	if err := ops.SetSeen(ctx, gone, true); !errors.Is(err, ErrMoved) {
		t.Fatalf("unknown UID: %v", err)
	}

	_, env, err := ops.List(ctx, "INBOX", 0, 10)
	if err != nil || len(env) != 2 || env[0].UID != b.UID || env[0].MessageID != "b@example.test" ||
		env[0].FromName != "Paul" || env[0].FromEmail != "paul@example.test" || env[0].Subject != "Sujet b" {
		t.Fatalf("list: %+v %v", env, err)
	}
	if _, env, _ := ops.List(ctx, "INBOX", b.UID, 10); len(env) != 1 || env[0].UID != a.UID {
		t.Fatalf("list before: %+v", env)
	}
}

func TestMoveArchiveTrash(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps imap.CapSet
	}{
		{"move", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}, imap.CapMove: {}}},
		{"copy+uidplus", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := startFolders(t, tc.caps, "Corbeille", "Classement")
			cfg := cfgFor(t, addr, "secret")
			ops := NewOps(cfg)
			ctx := context.Background()
			a, b := put(t, ops, "INBOX", "a"), put(t, ops, "INBOX", "b")

			if _, err := ops.Move(ctx, *a, Target{Role: "archive"}); err == nil || !strings.Contains(err.Error(), "archive_folder") {
				t.Fatalf("no archive folder must be refused clearly: %v", err)
			}
			to, err := ops.Move(ctx, *a, Target{Role: "trash"}) // found by its French name
			if err != nil || to.Folder != "Corbeille" || to.UID == 0 {
				t.Fatalf("trash: %+v %v", to, err)
			}
			if _, err := ops.Move(ctx, *to, Target{Role: "trash"}); !errors.Is(err, ErrSameFolder) {
				t.Fatalf("trash twice: %v", err)
			}
			back, err := ops.Move(ctx, *to, Target{Folder: "INBOX"}) // undo
			if err != nil || back.Folder != "INBOX" {
				t.Fatalf("undo: %+v %v", back, err)
			}
			cfg.ArchiveFolder = "Classement"
			ops = NewOps(cfg)
			if _, err := ops.Move(ctx, *b, Target{Role: "archive"}); err != nil {
				t.Fatal(err)
			}
			if _, err := ops.Move(ctx, *back, Target{Folder: "Nulle part"}); err == nil {
				t.Fatal("unknown folder accepted")
			}
			st, _ := ops.Status(ctx, "INBOX")
			if !slices.Equal(st.Present, []uint32{back.UID}) {
				t.Fatalf("inbox holds %v, want only %d", st.Present, back.UID)
			}
			if ok, _ := ops.HasMessage(ctx, "Classement", "<b@example.test>"); !ok {
				t.Fatal("b not archived")
			}
			if ok, _ := ops.HasMessage(ctx, "Corbeille", "<a@example.test>"); ok {
				t.Fatal("a still in the trash after undo")
			}
			folders, err := ops.Folders(ctx)
			if err != nil || len(folders) != 3 {
				t.Fatalf("folders: %+v %v", folders, err)
			}
			roles := map[string]string{}
			for _, f := range folders {
				roles[f.Name] = f.Role
			}
			if roles["INBOX"] != "inbox" || roles["Classement"] != "archive" || roles["Corbeille"] != "trash" {
				t.Fatalf("roles: %v", roles)
			}
		})
	}
}

// Without MOVE nor UIDPLUS, a move must not run a plain EXPUNGE.
func TestMoveWithoutUIDPlusLeavesOriginalFlagged(t *testing.T) {
	addr := startFolders(t, imap.CapSet{imap.CapIMAP4rev1: {}}, "Trash")
	cfg := cfgFor(t, addr, "secret")
	ops := NewOps(cfg)
	ctx := context.Background()
	a, other := put(t, ops, "INBOX", "a"), put(t, ops, "INBOX", "other")

	c, err := Dial(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.Login(cfg.Username, "secret").Wait()
	_, _ = c.Select("INBOX", nil).Wait()
	// Louis flagged another message \Deleted in another client.
	_ = c.Store(imap.UIDSetNum(imap.UID(other.UID)), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()

	if _, err := ops.Move(ctx, *a, Target{Role: "trash"}); err != nil {
		t.Fatal(err)
	}
	st, _ := ops.Status(ctx, "INBOX")
	if len(st.Present) != 2 {
		t.Fatalf("a plain EXPUNGE ran: inbox holds %v", st.Present)
	}
}
