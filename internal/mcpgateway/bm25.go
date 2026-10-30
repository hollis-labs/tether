package mcpgateway

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hollis-labs/libs/agent/core/toolselect"
)

// bm25Variant switches the index inputs the search-eval experiments compare (CW-20261024-0115).
// The zero value is the shipped behavior; tests set searchVariant to measure a change.
type bm25Variant struct {
	// ArgNames adds each tool's input-schema property names to its searchable text.
	ArgNames bool
	// ArgDescriptions also adds the property descriptions (implies ArgNames).
	ArgDescriptions bool
	// Stem appends light stems of the words in the query and in each tool's text, so
	// "messages" finds "message". It adds forms; it never replaces the original words.
	Stem bool
	// NameBoost repeats the words of a tool's name in its title so they weigh more.
	NameBoost int
}

// defaultNameBoost is how many extra times a tool's name words are weighed (CW-20261024-0115). Measured
// on the search-eval fixture and on 24 held-out queries written before the experiment: recall@5 0.812 to
// 0.841, MRR 0.709 to 0.737 overall, held-out MRR 0.717 to 0.790, with every exact-name and prefix case
// still first. x4 adds nothing on the held-out set. Argument names/descriptions and stemming were also
// tried and are not shipped: they lowered MRR or hard-query recall (see TestSearchEvalVariants).
const defaultNameBoost = 3

// searchVariant is the active index variant; it is the shipped default except while a test measures another.
var searchVariant = bm25Variant{NameBoost: defaultNameBoost}

// BM25ScoreAll ranks the candidates against query with the shared toolselect
// ranker (BM25 with inverse document frequency over name, title, description
// and tags, the name's words weighed extra (defaultNameBoost), an exact-name tier and a prefix tier ahead of it) and returns one
// integer score per candidate, in candidate order. Scores are rank-derived, not
// BM25 values: the best match gets the highest score and the order between any
// two tools is exactly the ranker's, which is what the service sorts on. Tools
// the ranker does not return score 0.
//
// The index is built over the candidates passed in, so inverse document
// frequency reflects the tools the caller can actually see. A candidate list
// the ranker rejects (empty or duplicate names) falls back to KeywordScore.
func BM25ScoreAll(query string, entries []Entry) []int {
	v := searchVariant
	tools := make([]toolselect.Tool, len(entries))
	for i, e := range entries {
		t := toolselect.Tool{Server: e.Origin, Name: e.Tool.Name, Title: e.Tool.Title, Description: e.Tool.Description, Tags: e.Tags}
		if v.ArgNames || v.ArgDescriptions {
			t.Arguments = toolArguments(e.Tool.InputSchema, v.ArgDescriptions)
		}
		if v.NameBoost > 0 {
			t.Title = strings.TrimSpace(t.Title + strings.Repeat(" "+strings.ReplaceAll(e.Tool.Name, "_", " "), v.NameBoost))
		}
		if v.Stem {
			t.Description = withStems(t.Description + " " + strings.ReplaceAll(e.Tool.Name, "_", " "))
		}
		tools[i] = t
	}
	if v.Stem && strings.ContainsAny(strings.TrimSpace(query), " \t") {
		// A single token may be a tool name or a name prefix; only phrases are expanded.
		query = withStems(query)
	}
	hits, err := toolselect.Rank(toolselect.Catalog{Tools: tools}, query, nil)
	if err != nil {
		return keywordScoreAll(query, entries)
	}
	position := make(map[string]int, len(hits))
	for i, h := range hits {
		position[h.Tool.Name] = i
	}
	scores := make([]int, len(entries))
	for i, e := range entries {
		if p, ok := position[e.Tool.Name]; ok {
			scores[i] = len(hits) - p // 1 for the last hit: the service treats 0 as "no match"
		}
	}
	return scores
}

func keywordScoreAll(query string, entries []Entry) []int {
	scores := make([]int, len(entries))
	for i, e := range entries {
		scores[i] = KeywordScore(query, e)
	}
	return scores
}

// SearchScorerEnv names the environment variable that selects the search
// scorer: "keyword" restores the original word-count scorer, anything else
// (including unset) uses BM25.
const SearchScorerEnv = "TETHER_MCP_SEARCH_SCORER"

// UseScorer sets the service's search scorer by name. "keyword" selects
// KeywordScore; every other name selects BM25.
func (s *Service) UseScorer(name string) {
	if strings.EqualFold(strings.TrimSpace(name), "keyword") {
		s.Score, s.ScoreAll = KeywordScore, nil
		return
	}
	s.Score, s.ScoreAll = nil, BM25ScoreAll
}

// toolArguments lists a tool's top-level input-schema properties as searchable metadata, in name
// order, capped so one tool with a huge schema cannot dominate the index.
func toolArguments(schema any, withDescriptions bool) []toolselect.Argument {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	props, _ := m["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 24 {
		names = names[:24]
	}
	out := make([]toolselect.Argument, 0, len(names))
	for _, name := range names {
		a := toolselect.Argument{Name: strings.ReplaceAll(name, "_", " ")}
		if withDescriptions {
			if prop, ok := props[name].(map[string]any); ok {
				if d, ok := prop["description"].(string); ok {
					a.Description = truncateRunes(d, 160)
				}
			}
		}
		out = append(out, a)
	}
	return out
}

// stem is a deliberately small suffix stripper (plurals, -ing, -ed) for search recall, not linguistics.
func stem(w string) string {
	n := len(w)
	switch {
	case n > 4 && strings.HasSuffix(w, "ies"):
		return w[:n-3] + "y"
	case n > 5 && strings.HasSuffix(w, "sses"):
		return w[:n-2]
	case n > 4 && strings.HasSuffix(w, "ing"):
		return w[:n-3]
	case n > 4 && strings.HasSuffix(w, "ed"):
		return w[:n-2]
	case n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		return w[:n-1]
	}
	return w
}

// withStems returns text followed by the stems of its words that differ from the word.
func withStems(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') })
	seen := map[string]bool{}
	var extra []string
	for _, w := range words {
		if st := stem(w); st != w && !seen[st] {
			seen[st] = true
			extra = append(extra, st)
		}
	}
	if len(extra) == 0 {
		return text
	}
	return text + " " + strings.Join(extra, " ")
}

// truncateRunes cuts s to at most n bytes without splitting a character.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
