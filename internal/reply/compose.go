package reply

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Addr is a mailbox (display name + address).
type Addr struct {
	Name  string
	Email string
}

// Original is the header data of the mail being answered. It comes from
// the stored headers, never from the body or from the LLM.
type Original struct {
	MessageID  string
	References []string
	From       Addr
	ReplyTo    []Addr
	To         []Addr
	Cc         []Addr
	Subject    string
	Date       time.Time
	QuoteText  string // cleaned text of the original, quoted below the reply
}

// Recipients computes who a reply goes to, from headers only:
// Reply-To if present, else From; with replyAll, the original To and Cc are
// added. Louis's own addresses are removed and duplicates collapsed.
func Recipients(o Original, self map[string]bool, replyAll bool) (to, cc []Addr) {
	seen := map[string]bool{}
	for k := range self {
		seen[strings.ToLower(k)] = true
	}
	add := func(dst *[]Addr, list ...Addr) {
		for _, a := range list {
			e := strings.ToLower(strings.TrimSpace(a.Email))
			if e == "" || seen[e] || !validAddress(e) {
				continue
			}
			seen[e] = true
			*dst = append(*dst, Addr{Name: a.Name, Email: e})
		}
	}
	primary := o.ReplyTo
	if len(primary) == 0 {
		primary = []Addr{o.From}
	}
	// Replying to a mail Louis sent himself: answer its recipients.
	if len(o.ReplyTo) == 0 && self[strings.ToLower(o.From.Email)] {
		primary = o.To
	}
	add(&to, primary...)
	if replyAll {
		add(&to, o.To...)
		add(&cc, o.Cc...)
	}
	return to, cc
}

func validAddress(e string) bool {
	a, err := mail.ParseAddress(e)
	return err == nil && a.Address == e && !strings.ContainsAny(e, "\r\n,;<>\"")
}

// ReplySubject prefixes "RE: " once.
func ReplySubject(s string) string {
	s = strings.TrimSpace(clean(s))
	l := strings.ToLower(s)
	for _, p := range []string{"re:", "re :", "aw:", "réf :", "ref:"} {
		if strings.HasPrefix(l, p) {
			return s
		}
	}
	return "RE: " + s
}

// clean removes CR/LF and other controls from a header value (header
// injection) .
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// Draft is a reply ready to be stored or sent.
type Draft struct {
	MessageID string // without brackets
	From      Addr
	To, Cc    []Addr
	Subject   string
	Raw       []byte // RFC 5322 message
}

// ErrNoRecipient means the headers yield no valid recipient.
var ErrNoRecipient = errors.New("aucun destinataire valide dans les en-têtes")

// Build composes the reply message: threading headers, quoted original,
// UTF-8 quoted-printable body.
func Build(o Original, from Addr, to, cc []Addr, body string, now time.Time, loc *time.Location) (*Draft, error) {
	if len(to) == 0 {
		return nil, ErrNoRecipient
	}
	domain := "mailpilot.invalid"
	if _, d, ok := strings.Cut(from.Email, "@"); ok && d != "" {
		domain = d
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	d := &Draft{MessageID: hex.EncodeToString(buf) + ".mailpilot@" + domain, From: from, To: to, Cc: cc, Subject: ReplySubject(o.Subject)}

	var refs []string
	for _, r := range o.References {
		if r = clean(strings.Trim(r, "<> ")); r != "" {
			refs = append(refs, "<"+r+">")
		}
	}
	orig := clean(strings.Trim(o.MessageID, "<> "))
	if orig != "" && !strings.HasPrefix(orig, "synthetic-") && !strings.HasPrefix(orig, "unparsed-") {
		refs = append(refs, "<"+orig+">")
	} else {
		orig = ""
	}

	text := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if q := strings.TrimSpace(o.QuoteText); q != "" {
		name := o.From.Name
		if name == "" {
			name = o.From.Email
		}
		text += "\n\n" + attribution(o.Date, name, loc) + "\n"
		for _, l := range strings.Split(q, "\n") {
			text += "> " + l + "\n"
		}
	}
	if err := d.write(orig, refs, text, now, loc); err != nil {
		return nil, err
	}
	return d, nil
}

// write renders the headers and the quoted-printable body into d.Raw.
func (d *Draft) write(inReplyTo string, refs []string, text string, now time.Time, loc *time.Location) error {
	var h bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&h, "%s: %s\r\n", k, v) }
	hdr("From", addrHeader(d.From))
	hdr("To", addrList(d.To))
	if len(d.Cc) > 0 {
		hdr("Cc", addrList(d.Cc))
	}
	hdr("Subject", mime.QEncoding.Encode("utf-8", d.Subject))
	hdr("Date", now.In(loc).Format(time.RFC1123Z))
	hdr("Message-ID", "<"+d.MessageID+">")
	if inReplyTo != "" {
		hdr("In-Reply-To", "<"+inReplyTo+">")
	}
	if len(refs) > 0 {
		hdr("References", strings.Join(refs, " "))
	}
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	hdr("User-Agent", "mailpilot")
	h.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&h)
	if _, err := qp.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n"))); err != nil {
		return err
	}
	if err := qp.Close(); err != nil {
		return err
	}
	h.WriteString("\r\n")
	d.Raw = h.Bytes()
	return nil
}

var frDays = []string{"dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."}
var frMonths = []string{"", "janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.", "déc."}

func attribution(t time.Time, name string, loc *time.Location) string {
	if t.IsZero() {
		return clean(name) + " a écrit :"
	}
	t = t.In(loc)
	return fmt.Sprintf("Le %s %d %s %d à %s, %s a écrit :", frDays[t.Weekday()], t.Day(), frMonths[t.Month()], t.Year(), t.Format("15:04"), clean(name))
}

func addrHeader(a Addr) string {
	return (&mail.Address{Name: clean(a.Name), Address: a.Email}).String()
}

func addrList(l []Addr) string {
	parts := make([]string, len(l))
	for i, a := range l {
		parts[i] = addrHeader(a)
	}
	return strings.Join(parts, ", ")
}

// Emails returns the bare addresses.
func Emails(l ...[]Addr) []string {
	var out []string
	for _, x := range l {
		for _, a := range x {
			out = append(out, a.Email)
		}
	}
	return out
}
