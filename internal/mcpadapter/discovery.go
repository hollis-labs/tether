package mcpadapter

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
)

// DiscoveryEntry is the index record for one upstream tool.
type DiscoveryEntry struct {
	ToolName    string   // canonical tool name (e.g. "clockwork_task_create")
	ServerID    string   // upstream server (e.g. "clockwork")
	Description string   // tool description
	Tags        []string // server-level tags from catalog (e.g. ["tasks","planning"])
	// ToolDef is the original mcp.Tool — stored so mux_discover can return the
	// full marshaled schema (Description, InputSchema) without re-encoding.
	ToolDef mcp.Tool
	// words is the pre-computed word set for keyword matching (lowercase).
	words map[string]struct{}
}

// DiscoveryIndex holds all upstream tool entries and supports keyword/intent
// search. It is safe for concurrent read after Build.
//
// v1 uses simple keyword matching: the query is split on whitespace, and each
// word is checked against the entry's description words + tag set. Entries are
// ranked by match count (descending), with ties broken alphabetically. No
// embeddings are required.
type DiscoveryIndex struct {
	mu      sync.RWMutex
	entries []DiscoveryEntry // sorted by ToolName after Build
}

// NewDiscoveryIndex returns an empty index.
func NewDiscoveryIndex() *DiscoveryIndex {
	return &DiscoveryIndex{}
}

// Build (re)populates the index from the registry. serverTags maps serverID
// → tag slice as loaded from the catalog. Safe to call multiple times.
func (idx *DiscoveryIndex) Build(registry *ToolRegistry, serverTags map[string][]string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	defs := registry.AllDefinitions()
	entries := make([]DiscoveryEntry, 0, len(defs))
	for _, def := range defs {
		rt, ok := registry.Lookup(def.Name)
		if !ok || rt.ServerID == "" {
			// Skip native mux tools — they're not proxied upstream tools.
			continue
		}

		tags := serverTags[rt.ServerID]

		e := DiscoveryEntry{
			ToolName:    def.Name,
			ServerID:    rt.ServerID,
			Description: def.Description,
			Tags:        tags,
			ToolDef:     def,
			words:       buildWordSet(def.Name, def.Description, tags),
		}
		entries = append(entries, e)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ToolName < entries[j].ToolName
	})
	idx.entries = entries
}

// SearchResult is one discovery hit returned by Search.
type SearchResult struct {
	ToolName    string   `json:"tool_name"`
	ServerID    string   `json:"server"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	// InputSchema is the marshaled mcp.Tool.InputSchema, ready for the LLM to
	// use when constructing a mux_call arguments object.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Score       int             `json:"score"` // match word count; 0 = unfiltered return
}

// Search returns up to limit entries matching the intent/category/tags query,
// along with the total number of matches before truncation. Callers compare
// totalMatches to len(results) to know whether to widen the query or raise limit.
// When all filter fields are empty, the top `limit` entries sorted by name
// are returned.
//
//   - intent: free-text; split into words; each word matched against description
//     and tool name (lowercase).
//   - category: matched against tags (exact, case-insensitive).
//   - tags: additional tag filters (comma-separated or slice); AND semantics
//     within tags, OR across intent words.
//   - limit: capped at 50; 0 means 10.
func (idx *DiscoveryIndex) Search(intent, category string, extraTags []string, limit int) ([]SearchResult, int) {
	return idx.search(intent, category, extraTags, limit, nil)
}

func (idx *DiscoveryIndex) search(intent, category string, extraTags []string, limit int, unavailable map[string]bool) ([]SearchResult, int) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	// Normalise all filter inputs.
	queryWords := tokenise(intent)
	catLower := strings.ToLower(strings.TrimSpace(category))
	var tagFilters []string
	for _, t := range extraTags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			tagFilters = append(tagFilters, t)
		}
	}

	type scored struct {
		entry DiscoveryEntry
		score int
	}

	results := make([]scored, 0, len(idx.entries))
	for _, e := range idx.entries {
		if unavailable[e.ServerID] {
			continue
		}
		// Category filter (hard AND).
		if catLower != "" && !hasTag(e.Tags, catLower) {
			continue
		}
		// Extra tag filters (hard AND for each).
		skip := false
		for _, tf := range tagFilters {
			if !hasTag(e.Tags, tf) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Intent word scoring.
		score := 0
		for w := range queryWords {
			if _, ok := e.words[w]; ok {
				score++
			}
		}
		// If intent was specified but nothing matched, skip.
		if len(queryWords) > 0 && score == 0 {
			continue
		}

		results = append(results, scored{entry: e, score: score})
	}

	// Sort: score desc, then name asc for stable output.
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].entry.ToolName < results[j].entry.ToolName
	})

	totalMatches := len(results)

	// Cap and convert.
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]SearchResult, len(results))
	for i, r := range results {
		// Marshal the full tool InputSchema as a raw JSON value.
		var schemaRaw json.RawMessage
		if raw, err := json.Marshal(r.entry.ToolDef.InputSchema); err == nil {
			schemaRaw = raw
		}
		out[i] = SearchResult{
			ToolName:    r.entry.ToolName,
			ServerID:    r.entry.ServerID,
			Description: r.entry.Description,
			Tags:        r.entry.Tags,
			InputSchema: schemaRaw,
			Score:       r.score,
		}
	}
	return out, totalMatches
}

// Len returns the number of indexed upstream tools.
func (idx *DiscoveryIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}

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

// hasTag reports whether tags contains target (case-insensitive).
func hasTag(tags []string, target string) bool {
	for _, t := range tags {
		if strings.ToLower(t) == target {
			return true
		}
	}
	return false
}
