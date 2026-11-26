package reply

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/mail"
)

var self = map[string]bool{"louis@example.test": true, "louis.alias@example.test": true}

func emails(l []Addr) string { return strings.Join(Emails(l), ",") }

func TestRecipients(t *testing.T) {
	o := Original{
		From: Addr{Name: "Pierre", Email: "Pierre@Client.example"},
		To:   []Addr{{Email: "louis@example.test"}, {Email: "paul@client.example"}},
		Cc:   []Addr{{Email: "LOUIS.ALIAS@example.test"}, {Email: "compta@client.example"}, {Email: "pierre@client.example"}},
	}
	to, cc := Recipients(o, self, false)
	if emails(to) != "pierre@client.example" || len(cc) != 0 {
		t.Fatalf("reply: %v %v", to, cc)
	}
	to, cc = Recipients(o, self, true)
	if emails(to) != "pierre@client.example,paul@client.example" || emails(cc) != "compta@client.example" {
		t.Fatalf("reply all: %v %v", to, cc)
	}
	o.ReplyTo = []Addr{{Email: "collect@evil.example"}}
	to, _ = Recipients(o, self, false)
	if emails(to) != "collect@evil.example" {
		t.Fatalf("reply-to: %v", to)
	}
	// Invalid or injected addresses are dropped.
	o.ReplyTo = []Addr{{Email: "a@b.test\r\nBcc: x@y"}, {Email: "not an address"}}
	if to, _ = Recipients(o, self, false); len(to) != 0 {
		t.Fatalf("invalid kept: %v", to)
	}
	// Own sent mail: answer its recipients.
	own := Original{From: Addr{Email: "louis@example.test"}, To: []Addr{{Email: "x@y.test"}}}
	if to, _ = Recipients(own, self, false); emails(to) != "x@y.test" {
		t.Fatalf("own: %v", to)
	}
}

func TestReplySubject(t *testing.T) {
	for in, want := range map[string]string{"Contrat": "RE: Contrat", "RE: Contrat": "RE: Contrat", "Re : x": "Re : x", "a\r\nBcc: x": "RE: a  Bcc: x"} {
		if got := ReplySubject(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestBuild(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Paris")
	o := Original{
		MessageID: "orig@client.example", References: []string{"root@client.example"},
		From: Addr{Name: "Pierre Martin", Email: "pierre@client.example"}, Subject: "Devis été",
		Date: time.Date(2026, 11, 26, 9, 0, 0, 0, loc), QuoteText: "Peux-tu valider ?\nMerci",
	}
	d, err := Build(o, Addr{Name: "Louis Dupont", Email: "louis@example.test"},
		[]Addr{{Name: "Pierre Martin", Email: "pierre@client.example"}}, nil, "Bonjour Pierre,\n\nC'est validé.\n\nLouis", time.Date(2026, 11, 26, 10, 0, 0, 0, loc), loc)
	if err != nil {
		t.Fatal(err)
	}
	r, err := mail.CreateReader(strings.NewReader(string(d.Raw)))
	if err != nil {
		t.Fatal(err)
	}
	h := r.Header
	subj, _ := h.Subject()
	irt, _ := h.MsgIDList("In-Reply-To")
	refs, _ := h.MsgIDList("References")
	to, _ := h.AddressList("To")
	if subj != "RE: Devis été" || irt[0] != "orig@client.example" || len(refs) != 2 || refs[1] != "orig@client.example" || to[0].Address != "pierre@client.example" {
		t.Fatalf("headers: %q %v %v %v", subj, irt, refs, to)
	}
	if h.Get("Bcc") != "" || h.Get("Cc") != "" {
		t.Fatal("no Bcc/Cc expected")
	}
	p, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 4096)
	n, _ := p.Body.Read(body)
	text := string(body[:n])
	if !strings.Contains(text, "C'est validé.") || !strings.Contains(text, "Le jeu. 26 nov. 2026 à 09:00, Pierre Martin a écrit :") || !strings.Contains(text, "> Peux-tu valider ?") {
		t.Fatalf("body:\n%s", text)
	}
	if _, err := Build(o, Addr{Email: "louis@example.test"}, nil, nil, "x", time.Now(), loc); err != ErrNoRecipient { //nolint:errorlint // sentinel
		t.Fatal("no recipient must fail")
	}
}
