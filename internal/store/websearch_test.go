package store

import (
	"context"
	"testing"
	"time"
)

func TestSearchMessages(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	a := msg("a@x", 1)
	a.Subject, a.TextClean = "Devis toiture", "Voici le devis pour la réfection du toit."
	b := msg("b@x", 2)
	b.Subject, b.TextClean = "Facture électricité", "Votre facture de novembre."
	b.From = Address{Name: "Énergie Plus", Email: "factures@energie.test"}
	b.ReceivedAt = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	c := msg("c@x", 3)
	c.Subject, c.TextClean = "Devis fenêtres", "Le devis 100% fenêtres."
	idA, _, _ := s.InsertMessage(ctx, a, nil, "")
	idB, _, _ := s.InsertMessage(ctx, b, nil, "")
	idC, _, _ := s.InsertMessage(ctx, c, nil, "")
	// Archived by Louis: still found.
	if err := s.Relocate(ctx, idC, "Archive", 9, 1); err != nil {
		t.Fatal(err)
	}
	ids := func(o SearchOptions) []int64 {
		t.Helper()
		items, err := s.SearchMessages(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	eq := func(got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}
	eq(ids(SearchOptions{Words: "devis"}), idC, idA)
	eq(ids(SearchOptions{Words: "devis toit"}), idA) // every word must match
	eq(ids(SearchOptions{From: "ÉNERGIE"}), idB)
	eq(ids(SearchOptions{From: "energie.test"}), idB)
	eq(ids(SearchOptions{From: "%"}))               // wildcards are literal
	eq(ids(SearchOptions{Words: `NEAR("x") OR *`})) // no FTS syntax
	eq(ids(SearchOptions{Before: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}), idB)
	eq(ids(SearchOptions{After: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Words: "devis"}), idC, idA)
	if got, _ := s.SearchMessages(ctx, SearchOptions{Words: "le la"}); got != nil {
		t.Fatalf("empty search must find nothing: %v", got)
	}
	items, _ := s.SearchMessages(ctx, SearchOptions{Words: "fenêtres"})
	if len(items) != 1 || items[0].Mailbox != "Archive" {
		t.Fatalf("%+v", items)
	}
}

func TestPendingRemindersAndReceivedSince(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	id1, _, _ := s.InsertMessage(ctx, msg("a@x", 1), nil, "")
	id2, _, _ := s.InsertMessage(ctx, msg("b@x", 2), nil, "")
	add := func(pk int64, idx int, text, due, status string) {
		t.Helper()
		if _, _, err := s.InsertAction(ctx, &Action{MessagePK: pk, Idx: idx, Text: text, DueDate: due, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	add(id1, 0, "proposé sans date", "", ActionProposed)
	add(id1, 1, "créé demain", "2026-11-27", ActionCreated)
	add(id1, 2, "créé plus tard", "2026-12-20", ActionCreated)
	add(id2, 0, "proposé tôt", "2026-11-26", ActionProposed)
	add(id2, 1, "ignoré", "2026-11-26", ActionDismissed)
	// A suspect mail's reminders are not shown.
	if err := s.SaveAnalysis(ctx, &Analysis{MessagePK: id2, Attempt: 1, Provider: "t", Status: "ok", Category: "spam_probable"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingReminders(ctx, "2026-11-27", 10)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range got {
		texts = append(texts, r.Text)
	}
	if len(texts) != 2 || texts[0] != "créé demain" || texts[1] != "proposé sans date" {
		t.Fatalf("reminders %q", texts)
	}
	since, _ := s.ReceivedSince(ctx, "INBOX", time.Date(2026, 11, 26, 10, 2, 0, 0, time.UTC), 10)
	if len(since) != 1 || since[0].ID != id2 {
		t.Fatalf("since %+v", since)
	}
}
