package mcpgateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Entry struct {
	Tool   *mcpsdk.Tool
	Origin string
	Tags   []string
}
type OriginStatus struct {
	ID                string         `json:"id"`
	Degraded          bool           `json:"degraded"`
	Status            string         `json:"status"`
	ToolCount         int            `json:"cataloged_tools"`
	InventoryExamined bool           `json:"inventory_examined"`
	EligibleTools     int            `json:"eligible_tools"`
	AvailableTools    int            `json:"available_tools"`
	HiddenTools       int            `json:"hidden_tools"`
	Exclusions        map[string]int `json:"exclusions"`
	Error             string         `json:"error,omitempty"`
	// Schema coverage over this origin's eligible tools (CW-20261024-0048): how
	// many declare an outputSchema, annotations, and a title. Typed APIs and good
	// search results depend on these; they are counts, never schema contents.
	OutputSchemaTools int `json:"output_schema_tools"`
	AnnotatedTools    int `json:"annotated_tools"`
	TitledTools       int `json:"titled_tools"`
}
type Snapshot struct {
	Entries    []Entry
	Origins    []OriginStatus
	Lint       []NameFinding
	Collisions []NameCollision
}
type Item struct {
	Name        string                  `json:"name"`
	Origin      string                  `json:"origin,omitempty"`
	Title       string                  `json:"title,omitempty"`
	Description string                  `json:"description,omitempty"`
	Annotations *mcpsdk.ToolAnnotations `json:"annotations,omitempty"`
	// RequiredArgs names the top-level required arguments; present in summary
	// detail so a caller can often call a tool without hydrating its schema.
	RequiredArgs []string `json:"required_args,omitempty"`
	// Example is a minimal call shape for schema detail: the schema's own first
	// example when it has one, else placeholders for the required arguments.
	Example      map[string]any `json:"example,omitempty"`
	InputSchema  any            `json:"inputSchema,omitempty"`
	OutputSchema any            `json:"outputSchema,omitempty"`
	Meta         mcpsdk.Meta    `json:"_meta,omitempty"`
	Icons        []mcpsdk.Icon  `json:"icons,omitempty"`
	Score        *int           `json:"score,omitempty"`
	Error        string         `json:"error,omitempty"`
}
type Result struct {
	Items        []Item `json:"items"`
	Returned     int    `json:"returned"`
	TotalMatches int    `json:"total_matches"`
	NextCursor   string `json:"next_cursor,omitempty"`
	Truncated    bool   `json:"truncated"`
	// TruncationReason is "limit" (the requested page size) or "bytes" (the
	// response byte cap cut the page at an item boundary).
	TruncationReason string `json:"truncation_reason,omitempty"`
	// OmittedNames lists requested names that were not hydrated because the byte
	// cap was reached; request them again. Names hydration has no cursor.
	OmittedNames []string `json:"omitted_names,omitempty"`
	// OriginCounts groups the returned items by origin. Items stay in rank order,
	// because rank carries meaning (and is what search telemetry links to).
	OriginCounts       map[string]int `json:"origin_counts,omitempty"`
	Complete           bool           `json:"complete"`
	UnavailableServers []string       `json:"unavailable_servers"`
}
type Request struct {
	Query   string   `json:"query,omitempty"`
	Names   []string `json:"names,omitempty"`
	Servers []string `json:"servers,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	Limit   int      `json:"limit,omitempty"`
	Cursor  string   `json:"cursor,omitempty"`
}

// Service is the shared semantic API. Snapshot is taken once per request;
// changing availability/schema/filter inputs invalidates pagination cursors.
// Score is the existing scorer seam pending go-toolselect's tagged release.
type Service struct {
	Selection Selection
	Policy    *Policy
	Snapshot  func() Snapshot
	Score     func(query string, entry Entry) int
	// ScoreAll, when set, replaces Score for search: it scores every candidate in
	// one call and returns one score per entry, in order. Corpus-relative scorers
	// (BM25's inverse document frequency) need the whole candidate set, which the
	// per-entry Score cannot see. A score of 0 excludes the entry.
	ScoreAll func(query string, entries []Entry) []int
	Dispatch func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error)
	// MaxResultBytes caps the encoded size of the items in one response; 0 means
	// DefaultMaxResultBytes. The first item is always returned, however large.
	MaxResultBytes int
}

// DefaultMaxResultBytes is the item-bytes cap for one discovery response. It
// matches the starting default of the gateway result cap (CW-20261024-0052).
const DefaultMaxResultBytes = 32 << 10

func availability(snapshot Snapshot) (map[string]bool, []string) {
	unavailable := map[string]bool{}
	ids := []string{}
	for _, origin := range snapshot.Origins {
		if origin.Status != "connected" {
			unavailable[origin.ID] = true
			ids = append(ids, origin.ID)
		}
	}
	sort.Strings(ids)
	return unavailable, ids
}
func (s *Service) List(req Request) (Result, error)   { return s.find(req, false) }
func (s *Service) Search(req Request) (Result, error) { return s.find(req, true) }
func (s *Service) find(req Request, search bool) (Result, error) {
	if search && strings.TrimSpace(req.Query) == "" {
		return Result{}, fmt.Errorf("query must be nonempty")
	}
	if search && req.Names != nil {
		return Result{}, fmt.Errorf("search does not accept names")
	}
	if !search && (req.Query != "" || req.Detail != "" || req.Tags != nil) {
		return Result{}, fmt.Errorf("list accepts names or enumeration filters only")
	}
	if req.Names != nil && (req.Servers != nil || req.Cursor != "" || req.Limit != 0) {
		return Result{}, fmt.Errorf("names hydration and enumeration fields are mutually exclusive")
	}
	maxLimit, defaultLimit := 100, 50
	if search {
		maxLimit, defaultLimit = 50, 10
		if req.Detail == "" {
			req.Detail = "summary"
		}
		if req.Detail != "names" && req.Detail != "summary" && req.Detail != "schema" {
			return Result{}, fmt.Errorf("detail must be names, summary or schema")
		}
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return Result{}, fmt.Errorf("limit must be 1..%d", maxLimit)
	}
	snapshot := s.Snapshot()
	if s.Policy != nil {
		snapshot = s.Policy.Eligible(snapshot)
	}
	unavailable, unavailableServers := availability(snapshot)
	knownServers := map[string]bool{}
	for _, origin := range snapshot.Origins {
		if s.Policy == nil || s.Policy.Selection.Profile == nil {
			knownServers[origin.ID] = true
		} else if origin.Status != "connected" && origin.Status != "excluded" {
			allowed := s.Policy.Selection.Profile.Servers == nil
			for _, id := range s.Policy.Selection.Profile.Servers {
				if id == origin.ID {
					allowed = true
				}
			}
			if allowed {
				knownServers[origin.ID] = true
			}
		}
	}
	for _, entry := range snapshot.Entries {
		knownServers[entry.Origin] = true
	}
	servers := map[string]bool{}
	for _, id := range req.Servers {
		if !knownServers[id] {
			return Result{}, fmt.Errorf("unknown or excluded server %q", id)
		}
		servers[id] = true
	}
	out := Result{Items: []Item{}, Complete: len(unavailableServers) == 0, UnavailableServers: unavailableServers}
	// list always hydrates real schemas; search honors names|summary|schema.
	detail := "schema"
	if search {
		detail = req.Detail
	}
	maxBytes := s.MaxResultBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResultBytes
	}
	itemFor := func(entry Entry, score *int) Item {
		switch detail {
		case "names":
			return Item{Name: entry.Tool.Name, Origin: entry.Origin, Score: score}
		case "summary":
			brief, _ := BriefDescription(entry.Tool.Description)
			return Item{Name: entry.Tool.Name, Origin: entry.Origin, Title: entry.Tool.Title, Description: brief, Annotations: entry.Tool.Annotations, RequiredArgs: requiredArgs(entry.Tool.InputSchema), Score: score}
		}
		tool := ClientSafeTool(entry.Tool)
		example, _ := SynthesizedExample(tool.InputSchema)
		return Item{Name: tool.Name, Origin: entry.Origin, Title: tool.Title, Description: tool.Description, Annotations: tool.Annotations, Icons: tool.Icons, Score: score, Example: example, InputSchema: tool.InputSchema, OutputSchema: tool.OutputSchema, Meta: tool.Meta}
	}
	itemBytes := func(item Item) int {
		raw, err := json.Marshal(item)
		if err != nil {
			return 0
		}
		return len(raw)
	}
	countOrigins := func(items []Item) map[string]int {
		counts := map[string]int{}
		for _, item := range items {
			if item.Error == "" && item.Origin != "" {
				counts[item.Origin]++
			}
		}
		if len(counts) == 0 {
			return nil
		}
		return counts
	}
	if req.Names != nil {
		if len(req.Names) > 100 {
			return Result{}, fmt.Errorf("names accepts at most 100 tools")
		}
		byName := map[string]Entry{}
		for _, entry := range snapshot.Entries {
			byName[entry.Tool.Name] = entry
		}
		used := 0
		for i, name := range req.Names {
			var item Item
			entry, ok := byName[name]
			switch {
			case !ok:
				item = Item{Name: name, Error: "unknown or excluded tool"}
			case unavailable[entry.Origin]:
				item = Item{Name: name, Origin: entry.Origin, Error: "origin unavailable"}
			default:
				item = itemFor(entry, nil)
			}
			size := itemBytes(item)
			if len(out.Items) > 0 && used+size > maxBytes {
				out.Truncated, out.TruncationReason = true, "bytes"
				out.OmittedNames = append([]string(nil), req.Names[i:]...)
				break
			}
			used += size
			out.Items = append(out.Items, item)
		}
		out.OriginCounts = countOrigins(out.Items)
		out.Returned = len(out.Items)
		out.TotalMatches = out.Returned
		return out, nil
	}
	type scored struct {
		entry Entry
		score int
	}
	matches := []scored{}
	candidates := []Entry{}
	for _, entry := range snapshot.Entries {
		if unavailable[entry.Origin] || (req.Servers != nil && !servers[entry.Origin]) {
			continue
		}
		tagsMatch := true
		for _, tag := range req.Tags {
			found := false
			for _, actual := range entry.Tags {
				if strings.EqualFold(tag, actual) {
					found = true
					break
				}
			}
			if !found {
				tagsMatch = false
				break
			}
		}
		if !tagsMatch {
			continue
		}
		candidates = append(candidates, entry)
	}
	var batch []int
	if search && s.ScoreAll != nil {
		if batch = s.ScoreAll(req.Query, candidates); len(batch) != len(candidates) {
			return Result{}, fmt.Errorf("scorer returned %d scores for %d tools", len(batch), len(candidates))
		}
	}
	for i, entry := range candidates {
		score := 0
		if search {
			if batch != nil {
				score = batch[i]
			} else {
				score = s.Score(req.Query, entry)
			}
			if score == 0 {
				continue
			}
		}
		matches = append(matches, scored{entry, score})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if !search && s.Policy != nil {
			return s.Policy.Less(matches[i].entry, matches[j].entry)
		}
		return matches[i].entry.Tool.Name < matches[j].entry.Tool.Name
	})
	cursor := req.Cursor
	req.Cursor = "" // cursor is not part of its own binding
	// Limit is deliberately bound, too; a page resumes exactly the same request.
	raw, _ := json.Marshal(struct {
		Selection Selection
		Snapshot  Snapshot
		Request   Request
		Search    bool
		Policy    *Policy
	}{s.Selection, snapshot, req, search, s.Policy})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(raw))
	offset, err := readCursor(cursor, fingerprint, len(matches))
	if err != nil {
		return Result{}, err
	}
	out.TotalMatches = len(matches)
	end := min(offset+limit, len(matches))
	used, cutByBytes := 0, false
	for _, match := range matches[offset:end] {
		var score *int
		if search {
			value := match.score
			score = &value
		}
		item := itemFor(match.entry, score)
		size := itemBytes(item)
		if len(out.Items) > 0 && used+size > maxBytes {
			cutByBytes = true
			break
		}
		used += size
		out.Items = append(out.Items, item)
	}
	end = offset + len(out.Items)
	out.Returned = len(out.Items)
	out.OriginCounts = countOrigins(out.Items)
	out.Truncated = end < len(matches)
	if out.Truncated {
		out.TruncationReason = "limit"
		if cutByBytes {
			out.TruncationReason = "bytes"
		}
		raw, _ := json.Marshal(pageCursor{fingerprint, end})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, nil
}

type pageCursor struct {
	Binding string `json:"binding"`
	Offset  int    `json:"offset"`
}

func readCursor(cursor, binding string, total int) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	var page pageCursor
	if err != nil || json.Unmarshal(raw, &page) != nil || page.Binding != binding || page.Offset < 0 || page.Offset > total {
		return 0, fmt.Errorf("invalid or stale cursor; restart discovery without a cursor")
	}
	return page.Offset, nil
}

// TargetError means no dispatch occurred. Transport errors from Dispatch retain
// their original protocol semantics and must not be disguised as target errors.
type TargetError struct {
	Message     string
	Unavailable bool
}

func (e *TargetError) Error() string { return e.Message }

func (s *Service) ResolveTarget(name string) (Entry, error) {
	snapshot := s.Snapshot()
	if s.Policy != nil {
		snapshot = s.Policy.Eligible(snapshot)
	}
	unavailable, _ := availability(snapshot)
	for _, entry := range snapshot.Entries {
		if entry.Tool.Name != name {
			continue
		}
		if unavailable[entry.Origin] {
			return Entry{}, &TargetError{Message: fmt.Sprintf("origin %q is unavailable; tool %q cannot be called", entry.Origin, name), Unavailable: true}
		}
		return entry, nil
	}
	return Entry{}, &TargetError{Message: fmt.Sprintf("tool %q is unknown or excluded", name)}
}

func (s *Service) Call(ctx context.Context, name string, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
	entry, err := s.ResolveTarget(name)
	if err != nil {
		return nil, err
	}
	result, err := s.Dispatch(ctx, name, args, meta)
	if err != nil || result == nil || !result.IsError {
		return result, err
	}
	return withArgHint(result, entry.Tool, args), nil
}

// withArgHint appends a gateway_hint content block to a FAILED result when the
// arguments sent do not fit the tool's schema (CW-20261024-0051). The upstream's
// own content stays first and untouched; the result is copied, never mutated,
// because a dispatcher may share it.
func withArgHint(result *mcpsdk.CallToolResult, tool *mcpsdk.Tool, args map[string]any) *mcpsdk.CallToolResult {
	hint := BuildArgHint(tool, args)
	if hint == nil {
		return result
	}
	raw, err := json.Marshal(struct {
		Hint *ArgHint `json:"gateway_hint"`
	}{hint})
	if err != nil {
		return result
	}
	out := *result
	out.Content = append(append([]mcpsdk.Content(nil), result.Content...), &mcpsdk.TextContent{Text: string(raw)})
	return &out
}

type Status struct {
	Selection
	Profile         string          `json:"profile,omitempty"`
	ProfileSource   string          `json:"profile_source,omitempty"`
	Warnings        []string        `json:"warnings,omitempty"`
	Lint            []NameFinding   `json:"lint"`
	Collisions      []NameCollision `json:"collisions"`
	Origins         []OriginStatus  `json:"origins"`
	CatalogedTools  int             `json:"cataloged_tools"`
	EligibleTools   int             `json:"eligible_tools"`
	AvailableTools  int             `json:"available_tools"`
	HiddenTools     int             `json:"hidden_tools"`
	UnexaminedTools int             `json:"unexamined_tools"`
	// Schema coverage totals over eligible tools; see OriginStatus.
	OutputSchemaTools int            `json:"output_schema_tools"`
	AnnotatedTools    int            `json:"annotated_tools"`
	TitledTools       int            `json:"titled_tools"`
	Exclusions        map[string]int `json:"exclusions"`
	Complete          bool           `json:"complete"`
	Name              string         `json:"name,omitempty"`
	Visible           *bool          `json:"visible,omitempty"`
	Reason            string         `json:"reason,omitempty"`
	Tool              *Item          `json:"tool,omitempty"`
}

func (s *Service) Status(name string) Status {
	snapshot := s.Snapshot()
	original := snapshot
	if s.Policy != nil {
		snapshot = s.Policy.Eligible(snapshot)
	}
	unavailable, ids := availability(snapshot)
	out := Status{Lint: original.Lint, Collisions: original.Collisions, Selection: s.Selection, Origins: append([]OriginStatus{}, snapshot.Origins...), EligibleTools: len(snapshot.Entries), Complete: len(ids) == 0 && len(original.Collisions) == 0, Name: name, Exclusions: map[string]int{}}
	byOrigin := map[string]int{}
	for i := range out.Origins {
		origin := &out.Origins[i]
		byOrigin[origin.ID] = i
		origin.EligibleTools = 0
		origin.AvailableTools = 0
		origin.HiddenTools = 0
		origin.OutputSchemaTools, origin.AnnotatedTools, origin.TitledTools = 0, 0, 0
		origin.Exclusions = map[string]int{}
	}
	if s.Policy != nil {
		for _, entry := range original.Entries {
			if reason := s.Policy.Exclusion(entry); reason != "" {
				out.HiddenTools++
				out.Exclusions[reason]++
				if i, ok := byOrigin[entry.Origin]; ok {
					out.Origins[i].HiddenTools++
					out.Origins[i].Exclusions[reason]++
				}
			}
		}
	}
	if s.Policy != nil && (s.Policy.Selection.Profile != nil || len(s.Policy.Floors) > 0) {
		visible := map[string]Entry{}
		for _, entry := range snapshot.Entries {
			visible[entry.Tool.Name] = entry
		}
		out.Lint = []NameFinding{}
		for _, finding := range original.Lint {
			if finding.Name == "" {
				// Origin-level findings reveal no tool names; include only selected origins.
				if _, ok := byOrigin[finding.Origin]; ok && s.Policy.originSelected(finding.Origin) {
					out.Lint = append(out.Lint, finding)
				}
			} else if entry, ok := visible[finding.Name]; ok && entry.Origin == finding.Origin {
				out.Lint = append(out.Lint, finding)
			}
		}
		out.Collisions = []NameCollision{}
		for _, collision := range original.Collisions {
			allowed := true
			for _, owner := range collision.Owners {
				entry := Entry{Origin: owner.Origin, Tool: &mcpsdk.Tool{Name: collision.Name}}
				if known, ok := visible[collision.Name]; ok && known.Origin == owner.Origin {
					entry = known
				}
				if !IsInfrastructure(collision.Name) && s.Policy.Exclusion(entry) != "" {
					allowed = false
				}
			}
			if allowed {
				out.Collisions = append(out.Collisions, collision)
			}
		}
		if len(out.Collisions) != len(original.Collisions) {
			out.Origins = append([]OriginStatus(nil), out.Origins...)
			for i := range out.Origins {
				if strings.Contains(out.Origins[i].Error, "collid") || strings.Contains(out.Origins[i].Error, "collision") {
					out.Origins[i].Error = "upstream naming collision; tool details excluded by profile"
				}
			}
		}
	}
	for _, finding := range out.Lint {
		if finding.Code == "conformance_unexamined" || finding.Code == "input_schema_unexamined" || finding.Code == "input_schema_limit" {
			out.UnexaminedTools++
		}
	}
	if s.Policy != nil {
		out.Profile = s.Policy.Selection.ID
		out.ProfileSource = s.Policy.Selection.Source
		out.Warnings = s.Policy.NameWarnings(original)
		if err := s.Policy.ValidateNames(original); err != nil {
			out.Warnings = append(out.Warnings, err.Error())
		}
		for _, id := range s.Policy.RestrictedOrigins {
			out.Warnings = append(out.Warnings, fmt.Sprintf("profile origin %s excluded by upstream restriction or confined grant", id))
		}
	}
	for _, origin := range snapshot.Origins {
		if origin.Degraded {
			out.Complete = false
		}
		out.CatalogedTools += origin.ToolCount
	}
	for _, entry := range snapshot.Entries {
		hasOutput, hasAnnotations, hasTitle := entry.Tool != nil && entry.Tool.OutputSchema != nil, entry.Tool != nil && entry.Tool.Annotations != nil, entry.Tool != nil && entry.Tool.Title != ""
		if hasOutput {
			out.OutputSchemaTools++
		}
		if hasAnnotations {
			out.AnnotatedTools++
		}
		if hasTitle {
			out.TitledTools++
		}
		if i, ok := byOrigin[entry.Origin]; ok {
			out.Origins[i].EligibleTools++
			if hasOutput {
				out.Origins[i].OutputSchemaTools++
			}
			if hasAnnotations {
				out.Origins[i].AnnotatedTools++
			}
			if hasTitle {
				out.Origins[i].TitledTools++
			}
			if !unavailable[entry.Origin] {
				out.Origins[i].AvailableTools++
			}
		}
		if !unavailable[entry.Origin] {
			out.AvailableTools++
		}
	}
	if name != "" {
		visible := false
		out.Visible = &visible
		out.Reason = "unknown or excluded by the upstream restriction"
		if s.Policy != nil {
			for _, entry := range original.Entries {
				if entry.Tool.Name == name {
					if reason := s.Policy.Exclusion(entry); reason != "" {
						out.Reason = reason
					}
					break
				}
			}
		}
		for _, entry := range snapshot.Entries {
			if entry.Tool.Name != name {
				continue
			}
			out.Tool = &Item{Name: name, Origin: entry.Origin, Title: entry.Tool.Title, Description: entry.Tool.Description, Annotations: entry.Tool.Annotations}
			switch {
			case unavailable[entry.Origin]:
				out.Reason = "origin unavailable"
			case s.Selection.Mode == Search:
				out.Reason = "search mode exposes discovery tools; hydrate this name with tether_tool_list and dispatch with tether_tool_call"
			default:
				visible = true
				out.Reason = "listed directly in flat mode"
			}
			break
		}
	}
	return out
}
