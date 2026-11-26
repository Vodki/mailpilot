package parse

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// quoteMarker separates a reply from the quoted history in converted HTML.
// CleanText cuts at it. It contains no character a real mail would carry.
const quoteMarker = "\x00MAILPILOT-QUOTE\x00"

// HTMLToText converts HTML to readable text. Links are rendered as their
// text only (URLs dropped), images and scripts removed. With markQuotes,
// Gmail/Outlook/Apple quote containers are replaced by quoteMarker.
func HTMLToText(src string, markQuotes bool) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}
	var b strings.Builder
	w := &textWriter{b: &b, markQuotes: markQuotes}
	w.walk(doc)
	return w.String()
}

type textWriter struct {
	b          *strings.Builder
	markQuotes bool
	quoted     bool // a quote marker was emitted: ignore the rest
	pre        int
}

func (w *textWriter) String() string { return w.b.String() }

func (w *textWriter) newline() {
	s := w.b.String()
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		w.b.WriteByte('\n')
	}
}

func (w *textWriter) paragraph() {
	w.newline()
	if !strings.HasSuffix(w.b.String(), "\n\n") && w.b.Len() > 0 {
		w.b.WriteByte('\n')
	}
}

func isQuoteContainer(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	id, class := attr(n, "id"), attr(n, "class")
	switch {
	case n.DataAtom == atom.Blockquote && (attr(n, "type") == "cite" || strings.Contains(class, "gmail_quote")):
		return true
	case strings.Contains(class, "gmail_quote"), strings.Contains(class, "yahoo_quoted"),
		strings.Contains(class, "moz-cite-prefix"), strings.Contains(class, "protonmail_quote"):
		return true
	case id == "divRplyFwdMsg", id == "appendonsend", strings.HasPrefix(id, "x_divRplyFwdMsg"):
		return true
	case n.DataAtom == atom.Div && strings.Contains(strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", ""), "border-top:solid#e1e1e1"):
		// Outlook desktop reply header block.
		return true
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (w *textWriter) walk(n *html.Node) {
	if w.quoted {
		return
	}
	if w.markQuotes && isQuoteContainer(n) {
		w.paragraph()
		w.b.WriteString(quoteMarker)
		w.quoted = true
		return
	}
	switch n.Type {
	case html.TextNode:
		t := n.Data
		if w.pre == 0 {
			t = collapseSpaces(t)
			if strings.HasSuffix(w.b.String(), "\n") || w.b.Len() == 0 {
				t = strings.TrimLeft(t, " ")
			}
		}
		w.b.WriteString(t)
		return
	case html.ElementNode:
		switch n.DataAtom {
		case atom.Script, atom.Style, atom.Head, atom.Title, atom.Img, atom.Svg, atom.Noscript, atom.Template:
			return
		case atom.Br:
			w.b.WriteByte('\n')
			return
		case atom.Hr:
			w.paragraph()
			w.b.WriteString("----")
			w.paragraph()
			return
		case atom.Pre:
			w.pre++
			defer func() { w.pre-- }()
		}
	}
	block := n.Type == html.ElementNode && isBlock(n.DataAtom)
	if block {
		w.newline()
	}
	if n.Type == html.ElementNode && n.DataAtom == atom.Li {
		w.b.WriteString("- ")
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}
	if n.Type == html.ElementNode {
		switch n.DataAtom {
		case atom.P, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Table, atom.Ul, atom.Ol, atom.Blockquote:
			w.paragraph()
		case atom.Td, atom.Th:
			w.b.WriteString(" ")
		default:
			if block {
				w.newline()
			}
		}
	}
}

func isBlock(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.Section, atom.Article, atom.Header, atom.Footer, atom.Li, atom.Ul, atom.Ol,
		atom.Tr, atom.Table, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Blockquote, atom.Pre,
		atom.Center, atom.Address, atom.Dl, atom.Dt, atom.Dd:
		return true
	}
	return false
}

func collapseSpaces(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', ' ', '\f':
			if !space {
				b.WriteByte(' ')
			}
			space = true
		default:
			b.WriteRune(r)
			space = false
		}
	}
	return b.String()
}
