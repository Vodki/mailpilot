package store

import (
	"context"
	"strings"
	"unicode"
)

// SearchHit is one result of a full-text search.
type SearchHit struct {
	MessagePK int64
	Summary   string
}

// stopwords are dropped from questions so that "qu'est-ce que Pierre a dit
// sur le devis" searches for pierre, dit, devis.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a à ai as au aux avec avait avoir c ça ce ces cet cette d dans de des du
		elle elles en est et été être il ils j je l la le les leur lui m ma mais me mes moi mon n ne ni nos
		notre nous on ont ou où par pas pour qu que quel quelle quelles quels qui quoi s sa sans se ses si son
		sont sur t ta te tes toi ton tu un une vos votre vous y est-ce quand comment combien pourquoi dit dis
		mail mails email emails message messages the of and to`) {
		stopwords[w] = true
	}
}

// SearchTerms splits a question into index terms (lower case, no
// stopwords, at most 12). Exported for tests.
func SearchTerms(q string) []string {
	var terms []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(w)) < 2 || stopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, w)
		if len(terms) == 12 {
			break
		}
	}
	return terms
}

// ftsQuery quotes every term, so no FTS5 syntax from the question survives,
// and allows prefixes ("factur" finds facture and factures).
func ftsQuery(terms []string) string {
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"*`
	}
	return strings.Join(parts, " OR ")
}

// Search returns the best matching messages, most relevant first; subject
// and sender weigh more than the body. An empty term list finds nothing.
func (s *Store) Search(ctx context.Context, question string, limit int) ([]SearchHit, error) {
	terms := SearchTerms(question)
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.rowid, f.summary FROM search_fts f JOIN messages m ON m.id = f.rowid
		WHERE search_fts MATCH ?
		ORDER BY bm25(search_fts, 4.0, 3.0, 1.0, 2.0), m.received_at DESC LIMIT ?`, ftsQuery(terms), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.MessagePK, &h.Summary); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// IndexedCount returns the number of indexed messages (/status).
func (s *Store) IndexedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM search_fts").Scan(&n)
	return n, err
}
