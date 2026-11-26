package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func migrated(t *testing.T) *Store {
	t.Helper()
	s := openTemp(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func msg(id string, uid uint32) *Message {
	return &Message{
		MessageID: id, Mailbox: "INBOX", UIDValidity: 7, UID: uid,
		From: Address{Name: "Pierre", Email: "pierre@x.test"}, Subject: "Sujet " + id,
		ReceivedAt: time.Date(2026, 11, 26, 10, int(uid), 0, 0, time.UTC),
		TextClean:  "corps " + id, TextFull: "corps complet " + id,
		To:          []Address{{Email: "louis@x.test"}},
		Attachments: []Attachment{{Filename: "a.pdf", MIMEType: "application/pdf", Size: 10}},
	}
}

func TestInsertDedupeAndThreads(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id1, ins, err := s.InsertMessage(ctx, msg("root@x", 1), nil, "sujet")
	if err != nil || !ins {
		t.Fatalf("insert: %v %v", ins, err)
	}
	// Same Message-ID under a new UID (e.g. after UIDVALIDITY change).
	dup := msg("root@x", 99)
	dup.UIDValidity = 8
	id2, ins, err := s.InsertMessage(ctx, dup, nil, "sujet")
	if err != nil || ins || id2 != id1 {
		t.Fatalf("dedupe: id %d/%d inserted %v err %v", id1, id2, ins, err)
	}
	got, err := s.GetMessage(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != 99 || got.UIDValidity != 8 || got.TextClean != "corps root@x" || len(got.Attachments) != 1 || got.To[0].Email != "louis@x.test" {
		t.Fatalf("locator/body not as expected: %+v", got)
	}

	reply := msg("reply@x", 2)
	idR, _, err := s.InsertMessage(ctx, reply, []string{"root@x"}, "sujet")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetMessage(ctx, idR)
	if r.ThreadID != got.ThreadID || r.ThreadID == 0 {
		t.Fatalf("reply thread %d, root thread %d", r.ThreadID, got.ThreadID)
	}
	// A reply that arrives before its unknown parent still groups with
	// later siblings through the root reference.
	orphan1 := msg("o1@x", 3)
	orphan2 := msg("o2@x", 4)
	i1, _, _ := s.InsertMessage(ctx, orphan1, []string{"missing-root@x"}, "autre")
	i2, _, _ := s.InsertMessage(ctx, orphan2, []string{"missing-root@x", "o1@x"}, "autre")
	m1, _ := s.GetMessage(ctx, i1)
	m2, _ := s.GetMessage(ctx, i2)
	if m1.ThreadID != m2.ThreadID || m1.ThreadID == r.ThreadID {
		t.Fatalf("orphan threads %d %d (root %d)", m1.ThreadID, m2.ThreadID, r.ThreadID)
	}
	thread, err := s.ThreadMessages(ctx, got.ThreadID, 10)
	if err != nil || len(thread) != 2 || thread[0].MessageID != "root@x" {
		t.Fatalf("thread %v %v", len(thread), err)
	}
}

func TestStateAndPending(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	silent := msg("b@x", 2)
	silent.State, silent.Delivery = StateSkipped, DeliverySilent
	_, _, _ = s.InsertMessage(ctx, silent, nil, "")
	ids, err := s.PendingIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("pending %v %v", ids, err)
	}
	if n, _ := s.IncAttempts(ctx, id); n != 1 {
		t.Fatalf("attempts %d", n)
	}
	_ = s.SetState(ctx, id, StateAnalyzed, "")
	_ = s.SetDelivery(ctx, id, DeliveryDigest)
	if ids, _ := s.PendingIDs(ctx); len(ids) != 0 {
		t.Fatalf("pending after done %v", ids)
	}
}

func TestAnalysesAndCost(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	if _, err := s.LatestAnalysis(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	_ = s.SaveAnalysis(ctx, &Analysis{MessagePK: id, Attempt: 1, Provider: "p", Status: "invalid", Error: "x"})
	_ = s.SaveAnalysis(ctx, &Analysis{MessagePK: id, Attempt: 2, Provider: "p", Status: "ok", Priority: "haute", Summary: "s", ResultJSON: `{"a":1}`, AIInstructions: true})
	a, err := s.LatestAnalysis(ctx, id)
	if err != nil || a.Priority != "haute" || !a.AIInstructions || a.Attempt != 2 {
		t.Fatalf("latest %+v %v", a, err)
	}
	_ = s.RecordLLMCall(ctx, LLMCall{Task: "triage", Provider: "p", MessagePK: id, CostUSD: 0.0123, Outcome: "ok"})
	_ = s.RecordLLMCall(ctx, LLMCall{Task: "triage", Provider: "p", CostUSD: 0.01, Outcome: "ok"})
	cost, n, err := s.LLMCostSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || n != 2 || cost < 0.0222 || cost > 0.0224 {
		t.Fatalf("cost %v n %d err %v", cost, n, err)
	}
}

func TestNotificationClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	ok, _ := s.ClaimNotification(ctx, id, "immediate")
	again, _ := s.ClaimNotification(ctx, id, "immediate")
	if !ok || again {
		t.Fatalf("claim %v again %v", ok, again)
	}
	_ = s.ReleaseNotification(ctx, id, "immediate")
	if ok, _ := s.ClaimNotification(ctx, id, "immediate"); !ok {
		t.Fatal("release should allow a new claim")
	}
	_ = s.SetNotificationMessage(ctx, id, "immediate", 42)
	_ = s.ReleaseNotification(ctx, id, "immediate") // no-op once sent
	if tg, _ := s.NotificationMessageID(ctx, id, "immediate"); tg != 42 {
		t.Fatalf("tg id %d", tg)
	}
}

func TestDigestBacklog(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	a, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	b, _, _ := s.InsertMessage(ctx, msg("b@x", 2), nil, "")
	_ = s.SetDelivery(ctx, a, DeliveryDigest)
	_ = s.SetDelivery(ctx, b, DeliveryDigest)
	_ = s.SaveAnalysis(ctx, &Analysis{MessagePK: a, Attempt: 1, Provider: "p", Status: "ok", Summary: "résumé"})
	items, err := s.DigestBacklog(ctx, 10)
	if err != nil || len(items) != 2 || items[0].Analysis == nil || items[1].Analysis != nil {
		t.Fatalf("backlog %+v %v", items, err)
	}
	id, ok, _ := s.ClaimDigest(ctx, "2026-11-26 08:30")
	if _, again, _ := s.ClaimDigest(ctx, "2026-11-26 08:30"); !ok || again {
		t.Fatal("slot must be claimed once")
	}
	_ = s.MarkDigested(ctx, id, []int64{a})
	items, _ = s.DigestBacklog(ctx, 10)
	if len(items) != 1 || items[0].Message.ID != b {
		t.Fatalf("after digest %+v", items)
	}
	st, err := s.GetStats(ctx, time.Time{})
	if err != nil || st.DigestBacklog != 1 || st.MessagesToday != 2 {
		t.Fatalf("stats %+v %v", st, err)
	}
}

func TestCallbacksSingleUseAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	tok, err := s.CreateCallback(ctx, "ignore", id, 0, "", time.Now().Add(time.Hour))
	if err != nil || len(tok) != 32 {
		t.Fatalf("token %q %v", tok, err)
	}
	c, err := s.ConsumeCallback(ctx, tok, time.Now())
	if err != nil || c.Action != "ignore" || c.MessagePK != id {
		t.Fatalf("consume %+v %v", c, err)
	}
	if _, err := s.ConsumeCallback(ctx, tok, time.Now()); !errors.Is(err, ErrCallbackUsed) {
		t.Fatalf("reuse: %v", err)
	}
	exp, _ := s.CreateCallback(ctx, "send", id, 0, "", time.Now().Add(time.Minute))
	if _, err := s.ConsumeCallback(ctx, exp, time.Now().Add(2*time.Minute)); !errors.Is(err, ErrCallbackExpired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := s.ConsumeCallback(ctx, "nope", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if n, _ := s.PurgeCallbacks(ctx, time.Now().Add(2*time.Hour)); n != 2 {
		t.Fatalf("purged %d", n)
	}
}

func TestPurgeBodiesKeepsMetadata(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	n, err := s.PurgeBodies(ctx, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
	m, _ := s.GetMessage(ctx, id)
	if m.TextClean != "" || !m.BodyPurged || m.Subject == "" {
		t.Fatalf("after purge %+v", m)
	}
}

func TestMailboxState(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	_, ok, err := s.GetMailboxState(ctx, "INBOX")
	if err != nil || ok {
		t.Fatalf("fresh state ok=%v err=%v", ok, err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	_ = s.SaveMailboxState(ctx, MailboxState{Mailbox: "INBOX", UIDValidity: 5, LastUID: 10, LastOK: now, BootstrapDone: true})
	st, ok, _ := s.GetMailboxState(ctx, "INBOX")
	if !ok || st.LastUID != 10 || !st.BootstrapDone || !st.LastOK.Equal(now) {
		t.Fatalf("state %+v", st)
	}
}

func TestActionsAndMutes(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	a := &Action{MessagePK: id, Idx: 0, Text: "Faire", Status: ActionProposed}
	aid, ins, err := s.InsertAction(ctx, a)
	if err != nil || !ins {
		t.Fatal(err)
	}
	if again, ins2, _ := s.InsertAction(ctx, &Action{MessagePK: id, Idx: 0, Text: "autre", Status: ActionProposed}); ins2 || again != aid {
		t.Fatal("action must be deduplicated on (message, idx)")
	}
	ok, _ := s.ClaimAction(ctx, aid, ActionProposed)
	twice, _ := s.ClaimAction(ctx, aid, ActionProposed)
	if !ok || twice {
		t.Fatal("claim must be exclusive")
	}
	_ = s.SetActionStatus(ctx, aid, ActionCreated, "T1", "")
	got, _ := s.GetAction(ctx, aid)
	if got.Status != ActionCreated || got.TodoistID != "T1" {
		t.Fatalf("%+v", got)
	}
	if open, _ := s.OpenActions(ctx, time.Now().Add(-time.Hour), 10); len(open) != 0 {
		t.Fatal("created action is not open")
	}

	_ = s.AddMute(ctx, "Spam.example", time.Time{})
	_ = s.AddMute(ctx, "old@x.test", time.Now().Add(-time.Hour))
	mutes, _ := s.Mutes(ctx, time.Now())
	if len(mutes) != 1 || mutes[0].Pattern != "spam.example" {
		t.Fatalf("mutes %+v", mutes)
	}
	if removed, _ := s.RemoveMute(ctx, "spam.example"); !removed {
		t.Fatal("remove")
	}
	_ = s.RecordRuleHits(ctx, id, []string{"r1", "r1"})
	if hits, _ := s.RuleHitCounts(ctx, time.Time{}); hits["r1"] != 1 {
		t.Fatalf("hits %v", hits)
	}
}
