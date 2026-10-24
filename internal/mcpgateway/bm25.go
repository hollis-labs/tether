package mcpgateway

import (
	"strings"

	"github.com/hollis-labs/libs/agent/core/toolselect"
)

// BM25ScoreAll ranks the candidates against query with the shared toolselect
// ranker (BM25 with inverse document frequency over name, title, description
// and tags, an exact-name tier and a prefix tier ahead of it) and returns one
// integer score per candidate, in candidate order. Scores are rank-derived, not
// BM25 values: the best match gets the highest score and the order between any
// two tools is exactly the ranker's, which is what the service sorts on. Tools
// the ranker does not return score 0.
//
// The index is built over the candidates passed in, so inverse document
// frequency reflects the tools the caller can actually see. A candidate list
// the ranker rejects (empty or duplicate names) falls back to KeywordScore.
func BM25ScoreAll(query string, entries []Entry) []int {
	tools := make([]toolselect.Tool, len(entries))
	for i, e := range entries {
		tools[i] = toolselect.Tool{Server: e.Origin, Name: e.Tool.Name, Title: e.Tool.Title, Description: e.Tool.Description, Tags: e.Tags}
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
