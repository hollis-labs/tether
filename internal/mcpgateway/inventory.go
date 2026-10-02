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
	ID             string `json:"id"`
	Status         string `json:"status"`
	ToolCount      int    `json:"cataloged_tools"`
	AvailableTools int    `json:"available_tools"`
	Error          string `json:"error,omitempty"`
}
type Snapshot struct {
	Entries []Entry
	Origins []OriginStatus
}
type Item struct {
	Name         string                  `json:"name"`
	Origin       string                  `json:"origin,omitempty"`
	Title        string                  `json:"title,omitempty"`
	Description  string                  `json:"description,omitempty"`
	Annotations  *mcpsdk.ToolAnnotations `json:"annotations"`
	InputSchema  any                     `json:"inputSchema,omitempty"`
	OutputSchema any                     `json:"outputSchema,omitempty"`
	Meta         mcpsdk.Meta             `json:"_meta,omitempty"`
	Icons        []mcpsdk.Icon           `json:"icons,omitempty"`
	Score        *int                    `json:"score,omitempty"`
	Error        string                  `json:"error,omitempty"`
}
type Result struct {
	Items              []Item   `json:"items"`
	Returned           int      `json:"returned"`
	TotalMatches       int      `json:"total_matches"`
	NextCursor         string   `json:"next_cursor,omitempty"`
	Truncated          bool     `json:"truncated"`
	Complete           bool     `json:"complete"`
	UnavailableServers []string `json:"unavailable_servers"`
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
	Dispatch  func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error)
}

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
		if req.Detail != "summary" && req.Detail != "schema" {
			return Result{}, fmt.Errorf("detail must be summary or schema")
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
	schema := !search || req.Detail == "schema"
	itemFor := func(entry Entry, score *int) Item {
		tool := entry.Tool
		item := Item{Name: tool.Name, Origin: entry.Origin, Title: tool.Title, Description: tool.Description, Annotations: tool.Annotations, Icons: tool.Icons, Score: score}
		if schema {
			item.InputSchema = tool.InputSchema
			item.OutputSchema = tool.OutputSchema
			item.Meta = tool.Meta
		}
		return item
	}
	if req.Names != nil {
		if len(req.Names) > 100 {
			return Result{}, fmt.Errorf("names accepts at most 100 tools")
		}
		byName := map[string]Entry{}
		for _, entry := range snapshot.Entries {
			byName[entry.Tool.Name] = entry
		}
		for _, name := range req.Names {
			entry, ok := byName[name]
			if !ok {
				out.Items = append(out.Items, Item{Name: name, Error: "unknown or excluded tool"})
				continue
			}
			if unavailable[entry.Origin] {
				out.Items = append(out.Items, Item{Name: name, Origin: entry.Origin, Error: "origin unavailable"})
				continue
			}
			out.Items = append(out.Items, itemFor(entry, nil))
		}
		out.Returned = len(out.Items)
		out.TotalMatches = out.Returned
		return out, nil
	}
	type scored struct {
		entry Entry
		score int
	}
	matches := []scored{}
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
		score := 0
		if search {
			score = s.Score(req.Query, entry)
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
	for _, match := range matches[offset:end] {
		var score *int
		if search {
			value := match.score
			score = &value
		}
		out.Items = append(out.Items, itemFor(match.entry, score))
	}
	out.Returned = len(out.Items)
	out.Truncated = end < len(matches)
	if out.Truncated {
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
type TargetError struct{ Message string }

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
			return Entry{}, &TargetError{fmt.Sprintf("origin %q is unavailable; tool %q cannot be called", entry.Origin, name)}
		}
		return entry, nil
	}
	return Entry{}, &TargetError{fmt.Sprintf("tool %q is unknown or excluded", name)}
}

func (s *Service) Call(ctx context.Context, name string, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
	if _, err := s.ResolveTarget(name); err != nil {
		return nil, err
	}
	return s.Dispatch(ctx, name, args, meta)
}

type Status struct {
	Selection
	Profile        string         `json:"profile,omitempty"`
	ProfileSource  string         `json:"profile_source,omitempty"`
	Warnings       []string       `json:"warnings,omitempty"`
	Origins        []OriginStatus `json:"origins"`
	CatalogedTools int            `json:"cataloged_tools"`
	EligibleTools  int            `json:"eligible_tools"`
	AvailableTools int            `json:"available_tools"`
	Complete       bool           `json:"complete"`
	Name           string         `json:"name,omitempty"`
	Visible        *bool          `json:"visible,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

func (s *Service) Status(name string) Status {
	snapshot := s.Snapshot()
	original := snapshot
	if s.Policy != nil {
		snapshot = s.Policy.Eligible(snapshot)
	}
	unavailable, ids := availability(snapshot)
	out := Status{Selection: s.Selection, Origins: snapshot.Origins, EligibleTools: len(snapshot.Entries), Complete: len(ids) == 0, Name: name}
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
		out.CatalogedTools += origin.ToolCount
	}
	for _, entry := range snapshot.Entries {
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
