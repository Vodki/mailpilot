package parse

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// Reply headers that introduce quoted history. Matched on a line that may
	// have been wrapped by the sender's client (joined first).
	replyHeaderRe = []*regexp.Regexp{
		regexp.MustCompile(`(?i)^le .{4,200}?a [ée]crit\s?:\s*$`),
		regexp.MustCompile(`(?i)^on .{4,200}?wrote\s?:\s*$`),
		regexp.MustCompile(`(?i)^am .{4,200}?schrieb .{0,100}:\s*$`),
		regexp.MustCompile(`(?i)^-{2,}\s*(original message|message d'origine|message d’origine|forwarded message|message transf[ée]r[ée])\s*-{2,}`),
		regexp.MustCompile(`(?i)^_{10,}\s*$`),
	}
	// Outlook header block: "De :" then, within the next lines, "Envoyé :" or
	// "Date :" and "À :" / "Objet :".
	outlookFromRe = regexp.MustCompile(`(?i)^\*?(de|from)\s?:\*?\s`)
	outlookNextRe = regexp.MustCompile(`(?i)^\*?(envoy[ée]|sent|date|[àa]|to|objet|subject|cc)\s?:\*?\s`)

	// Mobile and client signatures that end the useful text.
	signatureRe = []*regexp.Regexp{
		regexp.MustCompile(`^-- ?$`),
		regexp.MustCompile(`(?i)^(envoy[ée] (de|depuis) mon|sent from my|envoy[ée] avec|get outlook for|obtenir outlook pour|t[ée]l[ée]charger outlook)`),
	}
	blankLinesRe = regexp.MustCompile(`\n{3,}`)
)

// normalizeText unifies line endings, trims trailing spaces and collapses
// runs of blank lines.
func normalizeText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, " ", " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = strings.Join(lines, "\n")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// CleanText removes quoted history and signatures from a text body. It keeps
// the text before the first reply header, drops "> " lines and stops at a
// signature delimiter. If nothing remains, it returns the normalised input.
func CleanText(s string) string {
	s = normalizeText(s)
	if i := strings.Index(s, quoteMarker); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "\x00", "")
	lines := strings.Split(s, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		joined := line
		if i+1 < len(lines) {
			joined = line + " " + strings.TrimSpace(lines[i+1])
		}
		if isReplyHeader(line) || (!strings.HasPrefix(line, ">") && isReplyHeader(joined) && len(line) > 10) {
			break
		}
		if outlookFromRe.MatchString(line) && outlookBlockAt(lines, i) {
			break
		}
		if isSignature(line) {
			break
		}
		if strings.HasPrefix(line, ">") {
			continue
		}
		out = append(out, lines[i])
	}
	res := normalizeText(strings.Join(out, "\n"))
	if res == "" {
		return normalizeText(strings.ReplaceAll(s, quoteMarker, ""))
	}
	return res
}

func isReplyHeader(line string) bool {
	for _, re := range replyHeaderRe {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func isSignature(line string) bool {
	for _, re := range signatureRe {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func outlookBlockAt(lines []string, i int) bool {
	hits := 0
	for j := i + 1; j < len(lines) && j <= i+5; j++ {
		if outlookNextRe.MatchString(strings.TrimSpace(lines[j])) {
			hits++
		}
	}
	return hits >= 2
}
