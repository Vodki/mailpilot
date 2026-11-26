package store

import (
	"context"
	"testing"
)

func inboxIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	items, err := s.ListMessages(context.Background(), ListOptions{Mailbox: "INBOX", View: ViewInbox})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

func TestRelocateAndPresence(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	a, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "a")
	b, _, _ := s.InsertMessage(ctx, msg("b@x", 2), nil, "b")
	c, _, _ := s.InsertMessage(ctx, msg("c@x", 3), nil, "c")

	// Louis archives a from the web interface.
	if err := s.Relocate(ctx, a, "Archive", 9, 41); err != nil {
		t.Fatal(err)
	}
	if got := inboxIDs(t, s); len(got) != 2 {
		t.Fatalf("archived message still listed: %v", got)
	}
	if err := s.Relocate(ctx, 999, "Archive", 9, 1); err == nil {
		t.Fatal("unknown id accepted")
	}

	// Another client deleted b; c arrived after the server was read (uid 3
	// is not below uidNext 3) and must stay.
	v0, _ := s.ChangeVersion(ctx)
	n, err := s.SyncPresence(ctx, "INBOX", 7, 3, []uint32{})
	if err != nil || n != 1 {
		t.Fatalf("sync: %d %v", n, err)
	}
	if got := inboxIDs(t, s); len(got) != 1 || got[0] != c {
		t.Fatalf("inbox %v, want only %d", got, c)
	}
	counts, _ := s.ViewCounts(ctx, 1, "INBOX")
	if counts[ViewInbox] != 1 {
		t.Fatalf("counts %v", counts)
	}
	if v1, _ := s.ChangeVersion(ctx); v1 == v0 {
		t.Fatal("presence change must refresh the lists")
	}
	// Nothing changed: no write.
	if n, _ := s.SyncPresence(ctx, "INBOX", 7, 3, []uint32{}); n != 0 {
		t.Fatalf("idempotent sync wrote %d rows", n)
	}
	// Another UIDVALIDITY is not judged.
	if n, _ := s.SyncPresence(ctx, "INBOX", 8, 100, nil); n != 0 {
		t.Fatalf("other uidvalidity touched %d rows", n)
	}
	// b comes back (moved back by the other client): the watcher re-inserts it.
	back := msg("b@x", 12)
	if id, ins, _ := s.InsertMessage(ctx, back, nil, "b"); ins || id != b {
		t.Fatal("dedupe")
	}
	if got := inboxIDs(t, s); len(got) != 2 {
		t.Fatalf("returned message hidden: %v", got)
	}

	known, err := s.KnownByMessageID(ctx, []string{"a@x", "zz@x"})
	if err != nil || len(known) != 1 || known["a@x"].ID != a || known["a@x"].Mailbox != "Archive" || known["a@x"].UID != 41 {
		t.Fatalf("known: %+v %v", known, err)
	}
}
