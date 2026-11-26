// Package parse turns a raw RFC 5322 message into a normalised, UTF-8
// structure: decoded headers, readable text, cleaned text (quotes and
// signatures removed), threading identifiers and attachment metadata.
package parse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // ISO-8859-*, Windows-125x, ...
	"github.com/emersion/go-message/mail"
)

// Limits protect memory and the LLM context from pathological messages.
const (
	MaxRawSize  = 50 << 20 // bigger messages are stored as metadata only
	maxTextSize = 512 << 10
	maxParts    = 200
)

// Address is a decoded mailbox.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

// Attachment is metadata only; content is never stored.
type Attachment struct {
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Inline   bool   `json:"inline"`
}

// Message is the normalised form of an email.
type Message struct {
	MessageID     string // without angle brackets
	SyntheticID   bool   // true when the message had no Message-ID
	InReplyTo     []string
	References    []string
	From          Address
	ReplyTo       []Address
	To            []Address
	Cc            []Address
	Subject       string
	Date          time.Time
	ListID        string
	AutoSubmitted bool // Auto-Submitted / Precedence: bulk / list

	TextFull    string // whole readable body (HTML converted)
	TextClean   string // TextFull without quoted replies and signature
	HTMLOnly    bool
	Attachments []Attachment
	BodySHA256  string // of the raw message, for logs and dedupe
}

// HasRealAttachment reports an attachment that is not an inline image.
func (m *Message) HasRealAttachment() bool {
	for _, a := range m.Attachments {
		if !a.Inline {
			return true
		}
	}
	return false
}

// Parse decodes a raw message.
func Parse(raw []byte) (*Message, error) {
	sum := sha256.Sum256(raw)
	msg := &Message{BodySHA256: hex.EncodeToString(sum[:])}

	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if ent == nil {
		return nil, errors.New("parse: empty message")
	}
	h := mail.Header{Header: ent.Header}
	parseHeaders(&h, msg)

	var plain, html strings.Builder
	parts := 0
	walkErr := ent.Walk(func(_ []int, part *message.Entity, err error) error {
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return err
		}
		parts++
		if parts > maxParts {
			return errors.New("too many parts")
		}
		if part.MultipartReader() != nil {
			return nil
		}
		ct, ctParams, _ := part.Header.ContentType()
		if ct == "" {
			ct = "text/plain"
		}
		disp, dParams, _ := part.Header.ContentDisposition()
		filename := dParams["filename"]
		if filename == "" {
			filename = ctParams["name"]
		}
		if filename != "" {
			if dec, err := new(mime.WordDecoder).DecodeHeader(filename); err == nil {
				filename = dec
			}
		}
		isText := (ct == "text/plain" || ct == "text/html") && disp != "attachment" && filename == ""
		if !isText {
			if ct == "message/rfc822" && disp != "attachment" {
				// Forwarded message inline: its text is walked as children
				// by go-message only for multipart; treat as attachment.
				filename = firstNonEmpty(filename, "message.eml")
			}
			n, _ := io.Copy(io.Discard, io.LimitReader(part.Body, MaxRawSize))
			inline := disp == "inline" || (disp == "" && part.Header.Get("Content-Id") != "" && strings.HasPrefix(ct, "image/"))
			msg.Attachments = append(msg.Attachments, Attachment{
				Filename: firstNonEmpty(filename, "sans-nom"), MIMEType: ct, Size: n, Inline: inline,
			})
			return nil
		}
		body, _ := io.ReadAll(io.LimitReader(part.Body, maxTextSize))
		target := &plain
		if ct == "text/html" {
			target = &html
		}
		if target.Len() > 0 {
			target.WriteString("\n\n")
		}
		target.Write(body)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("parse: %w", walkErr)
	}

	switch {
	case strings.TrimSpace(plain.String()) != "":
		msg.TextFull = normalizeText(plain.String())
		msg.TextClean = CleanText(msg.TextFull)
		if html.Len() > 0 {
			// Prefer the HTML-side quote markers when the plain part has
			// none: Outlook often sends a plain part without "> ".
			if htmlClean := CleanText(HTMLToText(html.String(), true)); len(htmlClean) > 0 && len(htmlClean) < len(msg.TextClean) {
				msg.TextClean = htmlClean
			}
		}
	case html.Len() > 0:
		msg.HTMLOnly = true
		msg.TextFull = normalizeText(HTMLToText(html.String(), false))
		msg.TextClean = CleanText(HTMLToText(html.String(), true))
	}
	if strings.TrimSpace(msg.TextClean) == "" {
		msg.TextClean = msg.TextFull
	}
	return msg, nil
}

func parseHeaders(h *mail.Header, msg *Message) {
	if id, err := h.MessageID(); err == nil && id != "" {
		msg.MessageID = id
	}
	msg.InReplyTo, _ = h.MsgIDList("In-Reply-To")
	msg.References, _ = h.MsgIDList("References")
	if s, err := h.Subject(); err == nil {
		msg.Subject = strings.TrimSpace(s)
	} else {
		msg.Subject = strings.TrimSpace(h.Get("Subject"))
	}
	if d, err := h.Date(); err == nil {
		msg.Date = d
	}
	if l, err := h.AddressList("From"); err == nil && len(l) > 0 {
		msg.From = toAddr(l[0])
	}
	msg.ReplyTo = addrList(h, "Reply-To")
	msg.To = addrList(h, "To")
	msg.Cc = addrList(h, "Cc")
	msg.ListID = strings.Trim(strings.TrimSpace(h.Get("List-Id")), "<>")
	if lid := msg.ListID; strings.Contains(lid, "<") {
		// "Name <id>" form
		if i := strings.LastIndex(lid, "<"); i >= 0 {
			msg.ListID = strings.Trim(lid[i:], "<> ")
		}
	}
	auto := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted")))
	prec := strings.ToLower(strings.TrimSpace(h.Get("Precedence")))
	msg.AutoSubmitted = (auto != "" && auto != "no") || prec == "bulk" || prec == "list" || prec == "junk"

	if msg.MessageID == "" {
		msg.SyntheticID = true
		key := strings.Join([]string{msg.From.Email, h.Get("Date"), msg.Subject}, "\x00")
		sum := sha256.Sum256([]byte(key))
		msg.MessageID = "synthetic-" + hex.EncodeToString(sum[:16]) + "@mailpilot.invalid"
	}
}

func addrList(h *mail.Header, key string) []Address {
	l, err := h.AddressList(key)
	if err != nil {
		return nil
	}
	out := make([]Address, 0, len(l))
	for _, a := range l {
		out = append(out, toAddr(a))
	}
	return out
}

func toAddr(a *mail.Address) Address {
	return Address{Name: strings.TrimSpace(a.Name), Email: strings.ToLower(strings.TrimSpace(a.Address))}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ThreadKeys returns the identifiers that link a message to an existing
// thread, most significant first (root of References, then parents).
func (m *Message) ThreadKeys() []string {
	keys := append([]string{}, m.References...)
	keys = append(keys, m.InReplyTo...)
	return keys
}

// NormalizeSubject strips reply/forward prefixes for thread fallback.
func NormalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		l := strings.ToLower(s)
		cut := false
		for _, p := range []string{"re:", "re :", "tr:", "tr :", "fw:", "fwd:", "fw :", "aw:", "wg:", "réf:", "rép:", "rép :"} {
			if strings.HasPrefix(l, p) {
				s = strings.TrimSpace(s[len(p):])
				cut = true
				break
			}
		}
		if !cut {
			return strings.ToLower(s)
		}
	}
}
