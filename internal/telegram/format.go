package telegram

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxText is the Bot API limit for a message, in UTF-16 code units.
const MaxText = 4096

// Builder assembles a Text with bold/italic entities. Every string written is
// literal: there is no markup to escape because no parse_mode is used.
type Builder struct {
	b        strings.Builder
	n        int // UTF-16 length so far
	entities []Entity
}

func u16len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// clean removes characters Telegram rejects or that could spoof layout.
func clean(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f:
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi overrides
			return -1
		}
		return r
	}, s)
}

// Plain appends literal text.
func (t *Builder) Plain(s string) *Builder {
	s = clean(s)
	t.b.WriteString(s)
	t.n += u16len(s)
	return t
}

func (t *Builder) styled(kind, s string) *Builder {
	s = clean(s)
	if s == "" {
		return t
	}
	l := u16len(s)
	t.entities = append(t.entities, Entity{Type: kind, Offset: t.n, Length: l})
	t.b.WriteString(s)
	t.n += l
	return t
}

// Bold appends bold text.
func (t *Builder) Bold(s string) *Builder { return t.styled("bold", s) }

// Italic appends italic text.
func (t *Builder) Italic(s string) *Builder { return t.styled("italic", s) }

// Code appends monospace text.
func (t *Builder) Code(s string) *Builder { return t.styled("code", s) }

// Line appends literal text and a newline.
func (t *Builder) Line(s string) *Builder { return t.Plain(s + "\n") }

// Len returns the current UTF-16 length.
func (t *Builder) Len() int { return t.n }

// Text returns the message, truncated to MaxText.
func (t *Builder) Text() Text {
	body := strings.TrimRight(t.b.String(), "\n ")
	ents := t.entities
	if u16len(body) > MaxText {
		body, ents = truncateText(body, ents, MaxText-2)
		body += " …"
	}
	return Text{Body: body, Entities: ents}
}

func truncateText(s string, ents []Entity, limit int) (string, []Entity) {
	n := 0
	cut := len(s)
	for i, r := range s {
		l := utf16.RuneLen(r)
		if n+l > limit {
			cut = i
			break
		}
		n += l
	}
	var kept []Entity
	for _, e := range ents {
		if e.Offset >= n {
			continue
		}
		if e.Offset+e.Length > n {
			e.Length = n - e.Offset
		}
		kept = append(kept, e)
	}
	return s[:cut], kept
}

// Split breaks a long plain text into chunks under the limit, on line
// boundaries when possible (used for digests).
func Split(lines []Text, limit int) []Text {
	var out []Text
	var cur Builder
	for _, l := range lines {
		ll := u16len(l.Body) + 1
		if cur.Len() > 0 && cur.Len()+ll > limit {
			out = append(out, cur.Text())
			cur = Builder{}
		}
		base := cur.n
		for _, e := range l.Entities {
			e.Offset += base
			cur.entities = append(cur.entities, e)
		}
		cur.b.WriteString(l.Body + "\n")
		cur.n += ll
	}
	if cur.Len() > 0 {
		out = append(out, cur.Text())
	}
	return out
}
