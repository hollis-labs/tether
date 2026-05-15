package skills

import (
	"fmt"
	"sort"
	"strings"
)

// BrokerQuery is the caller-facing filter and ranking input for skill
// brokering. It keeps the interface close to the MCP surface so adapter code
// only needs to parse request args.
type BrokerQuery struct {
	Query    string
	Role     string
	Project  string
	TaskID   string
	Triggers []string
	Layers   []string
	Limit    int
}

// BrokerScore exposes the ranking components used to order broker results.
type BrokerScore struct {
	QueryMatches     int `json:"query_matches"`
	SignalMatches    int `json:"signal_matches"`
	PreferredMatches int `json:"preferred_matches"`
	Priority         int `json:"priority"`
}

// BrokerMatch is one ranked skill recommendation.
type BrokerMatch struct {
	LayeredSkill
	Score   BrokerScore
	Reasons []string
}

// BrokerResult holds the ranked matches and count metadata.
type BrokerResult struct {
	Matches      []BrokerMatch
	TotalVisible int
}

// BrokerLayered resolves every visible skill through the normal layered
// discovery path, applies broker filters, and returns the ranked subset.
func BrokerLayered(catalogRoot, workingDir string, query BrokerQuery) (BrokerResult, error) {
	all, err := DiscoverLayered(catalogRoot, workingDir)
	if err != nil {
		return BrokerResult{}, fmt.Errorf("discover skills: %w", err)
	}

	limit := query.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	layerFilters := normalizeStringSet(query.Layers)
	requestSignals := normalizeStringSet([]string{query.Role, query.Project})
	preferredWeights := makeOrderedWeightSet(query.Triggers)
	queryWords := tokenise(query.Query)
	hasRankingSignals := len(queryWords) > 0 || len(requestSignals) > 0 || len(preferredWeights) > 0

	matches := make([]BrokerMatch, 0, len(all))
	for _, candidate := range all {
		if len(layerFilters) > 0 {
			if _, ok := layerFilters[strings.ToLower(candidate.Layer)]; !ok {
				continue
			}
		}

		score := brokerScore(candidate.Skill, requestSignals, preferredWeights, queryWords)
		if hasRankingSignals && score.QueryMatches == 0 && score.SignalMatches == 0 && score.PreferredMatches == 0 {
			continue
		}

		matches = append(matches, BrokerMatch{
			LayeredSkill: candidate,
			Score:        score,
			Reasons:      brokerReasons(candidate, score, queryWords, requestSignals, preferredWeights),
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		left := matches[i].Score
		right := matches[j].Score
		switch {
		case left.QueryMatches != right.QueryMatches:
			return left.QueryMatches > right.QueryMatches
		case left.SignalMatches != right.SignalMatches:
			return left.SignalMatches > right.SignalMatches
		case left.PreferredMatches != right.PreferredMatches:
			return left.PreferredMatches > right.PreferredMatches
		case left.Priority != right.Priority:
			return left.Priority > right.Priority
		default:
			return matches[i].Skill.ID < matches[j].Skill.ID
		}
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}

	return BrokerResult{
		Matches:      matches,
		TotalVisible: len(all),
	}, nil
}

func brokerScore(s Skill, requestSignals map[string]struct{}, preferredWeights map[string]int, queryWords map[string]struct{}) BrokerScore {
	score := BrokerScore{Priority: s.Priority}
	signals := skillSignals(s)
	for _, signal := range signals {
		if _, ok := requestSignals[signal]; ok {
			score.SignalMatches++
		}
		if weight, ok := preferredWeights[signal]; ok {
			score.PreferredMatches += weight
		}
		if _, ok := queryWords[signal]; ok {
			score.QueryMatches++
		}
	}
	return score
}

func brokerReasons(candidate LayeredSkill, score BrokerScore, queryWords map[string]struct{}, requestSignals map[string]struct{}, preferredWeights map[string]int) []string {
	reasons := make([]string, 0, 5)
	signals := skillSignals(candidate.Skill)

	if matched := intersectNormalizedMap(signals, queryWords); len(matched) > 0 {
		reasons = append(reasons, "matched query: "+strings.Join(matched, ", "))
	}
	if matched := intersectNormalizedMap(signals, requestSignals); len(matched) > 0 {
		reasons = append(reasons, "matched role/project: "+strings.Join(matched, ", "))
	}
	if matched := intersectWeighted(signals, preferredWeights); len(matched) > 0 {
		reasons = append(reasons, "preferred triggers: "+strings.Join(matched, ", "))
	}
	if candidate.Layer != "" {
		reasons = append(reasons, "layer: "+candidate.Layer)
	}
	if score.Priority > 0 {
		reasons = append(reasons, fmt.Sprintf("priority: %d", score.Priority))
	}
	return reasons
}

func skillSignals(s Skill) []string {
	if len(s.Triggers) > 0 {
		return normalizeStringList(s.Triggers)
	}
	return tokeniseList(strings.Join([]string{s.ID, s.Name, s.Description}, " "))
}

func normalizeStringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range normalizeStringList(values) {
		out[value] = struct{}{}
	}
	return out
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func makeOrderedWeightSet(values []string) map[string]int {
	normalized := normalizeStringList(values)
	out := make(map[string]int, len(normalized))
	for i, value := range normalized {
		out[value] = len(normalized) - i
	}
	return out
}

func intersectNormalizedMap(values []string, allowed map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := allowed[value]; !ok {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func intersectWeighted(values []string, allowed map[string]int) []string {
	type weighted struct {
		value  string
		weight int
	}
	pairs := make([]weighted, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		weight, ok := allowed[value]
		if !ok {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		pairs = append(pairs, weighted{value: value, weight: weight})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].weight != pairs[j].weight {
			return pairs[i].weight > pairs[j].weight
		}
		return pairs[i].value < pairs[j].value
	})
	out := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, pair.value)
	}
	return out
}

func tokenise(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, token := range tokeniseList(s) {
		out[token] = struct{}{}
	}
	return out
}

func tokeniseList(s string) []string {
	var tokens []string
	isAlnum := func(r rune) bool { return 'a' <= r && r <= 'z' || '0' <= r && r <= '9' }
	for _, token := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isAlnum(r) }) {
		if len(token) <= 1 {
			continue
		}
		tokens = append(tokens, token)
	}
	return normalizeStringList(tokens)
}
