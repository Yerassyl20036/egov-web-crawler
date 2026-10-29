package store

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
)

// SearchParams are the inputs of a catalog search.
type SearchParams struct {
	Query    string // free text; Russian works best
	Category string // optional, substring of the category name (ru/kk/en)
	Agency   string // optional, substring or fuzzy match of the agency name
	Limit    int    // 1..50, default 10
}

// SearchHit is a dataset with its relevance score.
type SearchHit struct {
	catalog.Dataset
	Score float64 `json:"score"`
}

// ErrEmptyQuery is returned when neither a query nor a filter is given.
var ErrEmptyQuery = errors.New("store: empty search query")

// Search ranks datasets with a hybrid of Postgres full-text search
// (Russian stemming + prefix matching) and trigram similarity on titles,
// which rescues typos and partial words.
//
// The main signal is coverage: the share of distinct query terms found in
// the dataset's title, agency, keywords or category (head_vec). Plain
// ts_rank rewards repeated terms, which lets one of many regional copies
// of a dataset beat the copy from the region the user asked about.
// ts_rank, description coverage, title similarity and popularity break ties.
// The weights were tuned on evals/cases.json (see cmd/eval).
func (s *Store) Search(ctx context.Context, p SearchParams) ([]SearchHit, error) {
	if p.Limit <= 0 {
		p.Limit = 10
	}
	if p.Limit > 50 {
		p.Limit = 50
	}
	terms := QueryTerms(p.Query)
	tsq := strings.Join(terms, " | ")
	raw := strings.TrimSpace(p.Query)
	if tsq == "" && p.Category == "" && p.Agency == "" {
		return nil, ErrEmptyQuery
	}

	rows, err := s.pool.Query(ctx, `
WITH q AS (
	SELECT CASE WHEN $1 = '' THEN NULL::tsquery
	       ELSE to_tsquery('russian', $1) || to_tsquery('simple', $1) || to_tsquery('english', $1) END AS tsq,
	       ARRAY(SELECT to_tsquery('russian', t) || to_tsquery('simple', t) || to_tsquery('english', t)
	             FROM unnest($6::text[]) AS t) AS terms
)
SELECT `+selectColumns+`,
	(CASE WHEN q.tsq IS NULL THEN 0 ELSE
		(SELECT count(*) FROM unnest(q.terms) AS term WHERE d.head_vec @@ term)::float8 / cardinality(q.terms)
		+ 0.2 * (SELECT count(*) FROM unnest(q.terms) AS term WHERE d.body_vec @@ term)::float8 / cardinality(q.terms)
		+ 0.3 * ts_rank('{0.1, 0.2, 0.4, 1.0}', d.head_vec, q.tsq, 32)
	END)
	+ 0.3 * (CASE WHEN $2 = '' THEN 0 ELSE greatest(word_similarity($2, d.title_ru), word_similarity($2, d.title_kk)) END)
	+ 0.005 * ln(1 + d.views) AS score
FROM datasets d, q
WHERE (q.tsq IS NULL OR d.head_vec @@ q.tsq OR d.body_vec @@ q.tsq OR $2 <% d.title_ru OR $2 <% d.title_kk)
  AND ($3 = '' OR d.category_ru ILIKE '%' || $3 || '%' OR d.category_en ILIKE '%' || $3 || '%'
       OR d.category_kk ILIKE '%' || $3 || '%')
  AND ($4 = '' OR d.agency_ru ILIKE '%' || $4 || '%' OR d.agency_en ILIKE '%' || $4 || '%'
       OR d.agency_kk ILIKE '%' || $4 || '%' OR word_similarity($4, d.agency_ru) > 0.5)
ORDER BY score DESC, d.views DESC
LIMIT $5`, tsq, raw, strings.TrimSpace(p.Category), strings.TrimSpace(p.Agency), p.Limit, terms)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		d, err := scanDataset(rows, &h.Score)
		if err != nil {
			return nil, err
		}
		h.Dataset = d
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// BuildTSQuery turns free text into an OR of prefix terms, e.g.
// "больницы Алматы" -> "больницы:* | алматы:*". OR (not AND) keeps recall
// high for the agent's keyword-style queries; coverage ranking still puts
// documents that match every term first.
func BuildTSQuery(text string) string {
	return strings.Join(QueryTerms(text), " | ")
}

// QueryTerms splits free text into de-duplicated prefix terms ("больниц:*").
// Every token is letters or digits only, so each term is a valid
// to_tsquery input.
func QueryTerms(text string) []string {
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	var terms []string
	for _, t := range tokens {
		if len([]rune(t)) < 2 || stopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		terms = append(terms, t+":*")
		if len(terms) == 12 {
			break
		}
	}
	return terms
}

// Function words that would otherwise match almost every dataset when
// expanded as prefixes. Russian, Kazakh and English.
var stopwords = map[string]bool{
	"и": true, "в": true, "во": true, "на": true, "по": true, "для": true, "из": true, "за": true,
	"от": true, "до": true, "об": true, "о": true, "с": true, "со": true, "к": true, "не": true,
	"или": true, "как": true, "что": true, "это": true, "есть": true, "ли": true, "где": true,
	"какие": true, "какой": true, "мне": true, "все": true, "при": true,
	"мен": true, "және": true, "бойынша": true, "туралы": true, "қандай": true, "бар": true, "ма": true, "ме": true,
	"the": true, "of": true, "in": true, "and": true, "or": true, "for": true, "to": true, "on": true,
	"is": true, "are": true, "there": true, "any": true, "what": true, "which": true, "about": true,
	"me": true, "show": true, "find": true, "with": true, "by": true, "do": true, "you": true, "have": true,
}
