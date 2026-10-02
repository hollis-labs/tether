package mcpgateway

import "strings"

// ─── helpers ──────────────────────────────────────────────────────────────────

// buildWordSet unions the lowercase words from name, description, and tags.
func buildWordSet(name, desc string, tags []string) map[string]struct{} {
	ws := make(map[string]struct{})
	for w := range tokenise(name) {
		ws[w] = struct{}{}
	}
	for w := range tokenise(desc) {
		ws[w] = struct{}{}
	}
	for _, t := range tags {
		ws[strings.ToLower(t)] = struct{}{}
	}
	return ws
}

// tokenise splits s into a lowercase word set, dropping empty tokens.
func tokenise(s string) map[string]struct{} {
	ws := make(map[string]struct{})
	isAlnum := func(r rune) bool { return 'a' <= r && r <= 'z' || '0' <= r && r <= '9' }
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isAlnum(r) }) {
		if len(w) > 1 { // skip single-char noise
			ws[w] = struct{}{}
		}
	}
	return ws
}

// KeywordScore is the existing keyword scorer, shared by gateway transports.
// Exact names receive the existing bonus; tokenization/tag semantics are retained.
func KeywordScore(query string, entry Entry) int {
	if query == entry.Tool.Name {
		return len(tokenise(query)) + 1
	}
	words := buildWordSet(entry.Tool.Name, entry.Tool.Description, entry.Tags)
	score := 0
	for word := range tokenise(query) {
		if _, ok := words[word]; ok {
			score++
		}
	}
	return score

}
