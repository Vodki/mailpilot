package reply

import (
	"strings"
	"testing"
	"time"
)

func TestParseRecipients(t *testing.T) {
	got, err := ParseRecipients("Pierre Lambert <Pierre@Example.test>; marie@example.test, pierre@example.test")
	if err != nil || len(got) != 2 || got[0].Email != "pierre@example.test" || got[0].Name != "Pierre Lambert" || got[1].Email != "marie@example.test" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"pas une adresse", "a@b.test\r\nBcc: x@y.test", "<a@b.test>, nope"} {
		if _, err := ParseRecipients(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got, err := ParseRecipients("  "); err != nil || got != nil {
		t.Fatal("empty list")
	}
	many := strings.Repeat("a@b.test, ", 1) + strings.TrimSuffix(strings.Repeat("x@y.test, ", 1), ", ")
	if _, err := ParseRecipients(many); err != nil {
		t.Fatal(err)
	}
	var long []string
	for i := 0; i < MaxRecipients+1; i++ {
		long = append(long, "u"+string(rune('a'+i))+"@x.test")
	}
	if _, err := ParseRecipients(strings.Join(long, ",")); err == nil {
		t.Fatal("too many recipients accepted")
	}
}

func TestBuildNewAndForward(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Paris")
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	from := Addr{Name: "Louis", Email: "louis@example.test"}
	d, err := BuildNew(from, []Addr{{Email: "marie@example.test"}}, nil, "Dîner\r\nBcc: x@y.test", "Salut Marie", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(d.Raw)
	head, body, _ := strings.Cut(raw, "\r\n\r\n")
	if strings.Contains(head, "In-Reply-To") || strings.Contains(strings.ToLower(head), "\r\nbcc:") || !strings.Contains(head, "To: <marie@example.test>") ||
		!strings.Contains(body, "Salut Marie") {
		t.Fatalf("new:\n%s", raw)
	}
	o := Original{From: Addr{Name: "Pierre", Email: "pierre@example.test"}, Subject: "Devis", Date: now, QuoteText: "Voici le devis.",
		To: []Addr{{Email: "louis@example.test"}}}
	f, err := BuildForward(o, from, []Addr{{Email: "marie@example.test"}}, nil, ForwardSubject(o.Subject), "Pour info.", now, loc)
	if err != nil || f.Subject != "TR: Devis" || ForwardSubject("Fwd: x") != "Fwd: x" {
		t.Fatalf("%+v %v", f, err)
	}
	if fr := string(f.Raw); !strings.Contains(fr, "Message transf=C3=A9r=C3=A9") || !strings.Contains(fr, "Voici le devis.") || !strings.Contains(fr, "Pour info.") {
		t.Fatalf("forward:\n%s", fr)
	}
	if _, err := BuildNew(from, nil, nil, "x", "y", now, loc); err == nil {
		t.Fatal("no recipient accepted")
	}
}
