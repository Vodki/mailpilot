package reply

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// New messages and forwards. Their recipients are typed by Louis himself
// (never taken from a mail or from the LLM), parsed strictly here.

// MaxRecipients bounds the recipients of one message.
const MaxRecipients = 20

// ParseRecipients reads a comma-separated list typed by Louis
// ("Pierre <pierre@x.fr>, marie@y.fr"). Every address must be valid; an
// empty list is allowed (for Cc).
func ParseRecipients(s string) ([]Addr, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ";", ","))
	if s == "" {
		return nil, nil
	}
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("adresse invalide")
	}
	list, err := mail.ParseAddressList(s)
	if err != nil {
		return nil, fmt.Errorf("adresse invalide : %s", s)
	}
	seen := map[string]bool{}
	var out []Addr
	for _, a := range list {
		e := strings.ToLower(a.Address)
		if !validAddress(e) {
			return nil, fmt.Errorf("adresse invalide : %s", a.Address)
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, Addr{Name: clean(a.Name), Email: e})
	}
	if len(out) > MaxRecipients {
		return nil, fmt.Errorf("trop de destinataires (%d au plus)", MaxRecipients)
	}
	return out, nil
}

// ForwardSubject prefixes "TR: " once.
func ForwardSubject(s string) string {
	s = strings.TrimSpace(clean(s))
	l := strings.ToLower(s)
	for _, p := range []string{"tr:", "tr :", "fwd:", "fw:"} {
		if strings.HasPrefix(l, p) {
			return s
		}
	}
	return "TR: " + s
}

func newDraft(from Addr, to, cc []Addr, subject string) (*Draft, error) {
	if len(to) == 0 {
		return nil, errors.New("aucun destinataire")
	}
	domain := "mailpilot.invalid"
	if _, d, ok := strings.Cut(from.Email, "@"); ok && d != "" {
		domain = d
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return &Draft{MessageID: hex.EncodeToString(buf) + ".mailpilot@" + domain, From: from, To: to, Cc: cc,
		Subject: strings.TrimSpace(clean(subject))}, nil
}

// BuildNew composes a new message.
func BuildNew(from Addr, to, cc []Addr, subject, body string, now time.Time, loc *time.Location) (*Draft, error) {
	d, err := newDraft(from, to, cc, subject)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n")) + "\n"
	return d, d.write("", nil, text, now, loc)
}

// BuildForward composes a forward: Louis's text, then the original's
// headers and text (o.QuoteText). Attachments are not forwarded.
func BuildForward(o Original, from Addr, to, cc []Addr, subject, body string, now time.Time, loc *time.Location) (*Draft, error) {
	d, err := newDraft(from, to, cc, subject)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n")))
	b.WriteString("\n\n---------- Message transféré ----------\n")
	b.WriteString("De : " + clean(addrHeader(o.From)) + "\n")
	if !o.Date.IsZero() {
		b.WriteString("Date : " + o.Date.In(loc).Format("02/01/2006 15:04") + "\n")
	}
	b.WriteString("Objet : " + clean(o.Subject) + "\n")
	if len(o.To) > 0 {
		b.WriteString("À : " + clean(addrList(o.To)) + "\n")
	}
	b.WriteString("\n" + strings.TrimSpace(strings.ReplaceAll(o.QuoteText, "\r\n", "\n")) + "\n")
	return d, d.write("", nil, b.String(), now, loc)
}
