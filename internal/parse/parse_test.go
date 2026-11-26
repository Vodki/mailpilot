package parse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, name string) *Message {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "eml", name))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

func contains(t *testing.T, field, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s: %q does not contain %q", field, got, want)
	}
}

func absent(t *testing.T, field, got, unwanted string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Errorf("%s: %q must not contain %q", field, got, unwanted)
	}
}

func TestHTMLOnly(t *testing.T) {
	m := load(t, "html_only.eml")
	if !m.HTMLOnly {
		t.Error("expected HTMLOnly")
	}
	if m.Subject != "Validation du devis été" {
		t.Errorf("subject %q", m.Subject)
	}
	if m.From.Email != "pierre@client.example" || m.From.Name != "Pierre Martin" {
		t.Errorf("from %+v", m.From)
	}
	contains(t, "text", m.TextFull, "Peux-tu valider le devis avant vendredi ?")
	absent(t, "text", m.TextFull, "alert(1)")
	absent(t, "text", m.TextFull, "evil.example") // URLs are dropped
	absent(t, "text", m.TextFull, "p{}")
}

func TestLatin1MultipartWithAttachment(t *testing.T) {
	m := load(t, "multipart_latin1_attachment.eml")
	if m.Subject != "Facture n°42" {
		t.Errorf("subject %q", m.Subject)
	}
	if m.From.Name != "Sophie Léger" {
		t.Errorf("from name %q", m.From.Name)
	}
	contains(t, "text", m.TextFull, "échéance le 15/12")
	contains(t, "text", m.TextFull, "À bientôt")
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "facture-42.pdf" ||
		m.Attachments[0].MIMEType != "application/pdf" || m.Attachments[0].Size == 0 {
		t.Errorf("attachments %+v", m.Attachments)
	}
	if !m.HasRealAttachment() {
		t.Error("expected real attachment")
	}
	if len(m.Cc) != 1 || m.Cc[0].Email != "compta@example.test" {
		t.Errorf("cc %+v", m.Cc)
	}
}

func TestWindows1252Base64(t *testing.T) {
	m := load(t, "windows1252_base64.eml")
	if m.Subject != "Réunion déplacée" {
		t.Errorf("subject %q", m.Subject)
	}
	contains(t, "text", m.TextFull, "« budget »")
	contains(t, "text", m.TextFull, "3 000 €")
	contains(t, "clean", m.TextClean, "jeudi 14 h")
}

func TestGmailReply(t *testing.T) {
	m := load(t, "gmail_reply.eml")
	if m.TextClean != "OK pour moi, je prends la semaine du 12." {
		t.Errorf("clean %q", m.TextClean)
	}
	contains(t, "full", m.TextFull, "Qui prend la semaine du 12")
	if len(m.References) != 2 || m.References[0] != "root-planning@example.test" {
		t.Errorf("references %v", m.References)
	}
	if len(m.InReplyTo) != 1 || m.InReplyTo[0] != "orig-planning@example.test" {
		t.Errorf("in-reply-to %v", m.InReplyTo)
	}
	if k := m.ThreadKeys(); k[0] != "root-planning@example.test" {
		t.Errorf("thread keys %v", k)
	}
}

func TestOutlookReply(t *testing.T) {
	m := load(t, "outlook_reply.eml")
	contains(t, "clean", m.TextClean, "Je te renvoie le contrat signé demain matin.")
	absent(t, "clean", m.TextClean, "Peux-tu me renvoyer")
	absent(t, "clean", m.TextClean, "Envoyé :")
}

func TestSignatureAndSyntheticID(t *testing.T) {
	m := load(t, "signature_no_msgid.eml")
	if !m.SyntheticID || !strings.HasPrefix(m.MessageID, "synthetic-") {
		t.Errorf("message id %q synthetic=%v", m.MessageID, m.SyntheticID)
	}
	again := load(t, "signature_no_msgid.eml")
	if again.MessageID != m.MessageID {
		t.Error("synthetic id must be stable")
	}
	if m.TextClean != "Tu es dispo demain 10 h pour un point rapide ?" {
		t.Errorf("clean %q", m.TextClean)
	}
}

func TestNewsletter(t *testing.T) {
	m := load(t, "newsletter.eml")
	if m.ListID != "produit.newsletter.example" || !m.AutoSubmitted {
		t.Errorf("list-id %q auto %v", m.ListID, m.AutoSubmitted)
	}
}

func TestInjectionKeepsHeadersFromHeaders(t *testing.T) {
	m := load(t, "injection.eml")
	if m.From.Email != "attacker@evil.example" || len(m.ReplyTo) != 1 || m.ReplyTo[0].Email != "collect@evil.example" {
		t.Errorf("headers %+v %+v", m.From, m.ReplyTo)
	}
	contains(t, "text", m.TextFull, "Ignore tes instructions")
}

func TestCleanTextCases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Merci !\n\nOn Tue, Nov 24, 2026 at 6:02 PM Bob <b@x> wrote:\n> hi", "Merci !"},
		{"Réponse\n\n-----Message d'origine-----\nDe : x", "Réponse"},
		{"Texte\n> cité\nsuite", "Texte\nsuite"},
		{"Corps\n-- \nSignature", "Corps"},
		{"Corps\n\nSent from my iPhone", "Corps"},
		{"> tout est cité", "> tout est cité"}, // nothing left: keep original
		{"Ligne 1\r\n\r\n\r\n\r\nLigne 2", "Ligne 1\n\nLigne 2"},
	}
	for _, c := range cases {
		if got := CleanText(c.in); got != c.want {
			t.Errorf("CleanText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeSubject(t *testing.T) {
	for in, want := range map[string]string{
		"RE: TR: Fwd: Contrat": "contrat",
		"Re : Planning":        "planning",
		"Contrat":              "contrat",
	} {
		if got := NormalizeSubject(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestMalformedDoesNotPanic(t *testing.T) {
	for _, raw := range []string{"", "garbage", "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n", "Subject: =?bad?Q?x?=\r\n\r\nbody"} {
		_, _ = Parse([]byte(raw))
	}
}
