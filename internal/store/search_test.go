package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSearchTerms(t *testing.T) {
	got := SearchTerms(`Qu'est-ce que Pierre a dit sur le DEVIS "toiture" ? NEAR(x) OR *`)
	want := []string{"pierre", "devis", "toiture", "near", "or"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terms %q, want %q", got, want)
	}
	if q := ftsQuery(got); q != `"pierre"* OR "devis"* OR "toiture"* OR "near"* OR "or"*` {
		t.Fatalf("fts query %q", q)
	}
}

func TestSearchIndexFollowsData(t *testing.T) {
	ctx := context.Background()
	s := migrated(t)
	a := msg("a@x", 1)
	a.Subject, a.TextClean = "Devis toiture", "Voici le devis pour la réfection du toit."
	b := msg("b@x", 2)
	b.Subject, b.TextClean = "Facture électricité", "Votre facture de novembre est disponible."
	b.From = Address{Name: "Énergie Plus", Email: "factures@energie.test"}
	idA, _, _ := s.InsertMessage(ctx, a, nil, "devis toiture")
	idB, _, _ := s.InsertMessage(ctx, b, nil, "facture electricite")

	hits, err := s.Search(ctx, "où en est la refection ?", 5)
	if err != nil || len(hits) != 1 || hits[0].MessagePK != idA {
		t.Fatalf("accent-insensitive body search: %+v %v", hits, err)
	}
	hits, _ = s.Search(ctx, "factur energie", 5)
	if len(hits) != 1 || hits[0].MessagePK != idB {
		t.Fatalf("prefix and sender search: %+v", hits)
	}
	if hits, _ := s.Search(ctx, "le la de ?", 5); hits != nil {
		t.Fatalf("stopwords only must find nothing: %+v", hits)
	}

	if err := s.SaveAnalysis(ctx, &Analysis{MessagePK: idB, Attempt: 1, Provider: "t", Status: "ok", Summary: "Échéance du prélèvement le 5 décembre."}); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.Search(ctx, "prélèvement", 5)
	if len(hits) != 1 || hits[0].MessagePK != idB || hits[0].Summary == "" {
		t.Fatalf("summary indexed: %+v", hits)
	}

	// Purge: the body leaves the index, subject and summary stay.
	if _, err := s.PurgeBodies(ctx, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, "réfection", 5); len(hits) != 0 {
		t.Fatalf("purged body still searchable: %+v", hits)
	}
	if hits, _ := s.Search(ctx, "toiture prélèvement", 5); len(hits) != 2 {
		t.Fatalf("subject/summary lost after purge: %+v", hits)
	}
	if n, _ := s.IndexedCount(ctx); n != 2 {
		t.Fatalf("indexed %d", n)
	}
}
