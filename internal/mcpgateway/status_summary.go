package mcpgateway

import (
	"fmt"
	"strconv"
	"strings"
)

// OriginBrief is one origin in the compact status: enough to see what works.
type OriginBrief struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Available int    `json:"available_tools"`
	Cataloged int    `json:"cataloged_tools"`
	Degraded  bool   `json:"degraded,omitempty"`
	Error     string `json:"error,omitempty"`
	// Tools with an outputSchema / annotations (see OriginStatus).
	OutputSchema int `json:"output_schema_tools"`
	Annotated    int `json:"annotated_tools"`
}

// SchemaCoverage counts eligible tools that declare each optional schema aspect.
type SchemaCoverage struct {
	Tools            int `json:"tools"`
	WithOutputSchema int `json:"with_output_schema"`
	WithAnnotations  int `json:"with_annotations"`
	WithTitle        int `json:"with_title"`
}

// StatusSummary is the default answer of tether_gateway_status: counts, which
// origins work and which do not (with the reason), lint totals by code, and one
// line saying what "complete" means here. Lint detail is only on request.
type StatusSummary struct {
	Selection
	Profile            string         `json:"profile,omitempty"`
	Complete           bool           `json:"complete"`
	Explanation        string         `json:"explanation"`
	CatalogedTools     int            `json:"cataloged_tools"`
	EligibleTools      int            `json:"eligible_tools"`
	AvailableTools     int            `json:"available_tools"`
	HiddenTools        int            `json:"hidden_tools"`
	UnexaminedTools    int            `json:"unexamined_tools"`
	Exclusions         map[string]int `json:"exclusions"`
	Connected          []string       `json:"connected"`
	UnavailableServers []string       `json:"unavailable_servers"`
	Origins            []OriginBrief  `json:"origins"`
	SchemaCoverage     SchemaCoverage `json:"schema_coverage"`
	LintTotal          int            `json:"lint_total"`
	LintByCode         map[string]int `json:"lint_by_code"`
	Collisions         int            `json:"collisions"`
	CollisionNames     []string       `json:"collision_names,omitempty"`
	Warnings           []string       `json:"warnings,omitempty"`
	Hint               string         `json:"hint"`
	Name               string         `json:"name,omitempty"`
	Visible            *bool          `json:"visible,omitempty"`
	Reason             string         `json:"reason,omitempty"`
	Tool               *Item          `json:"tool,omitempty"`
}

const maxBriefError = 200

// Summary condenses a full Status. It is a pure view: nothing is recomputed.
func (st Status) Summary() StatusSummary {
	out := StatusSummary{
		Selection: st.Selection, Profile: st.Profile, Complete: st.Complete,
		CatalogedTools: st.CatalogedTools, EligibleTools: st.EligibleTools, AvailableTools: st.AvailableTools,
		HiddenTools: st.HiddenTools, UnexaminedTools: st.UnexaminedTools, Exclusions: st.Exclusions,
		SchemaCoverage: SchemaCoverage{Tools: st.EligibleTools, WithOutputSchema: st.OutputSchemaTools, WithAnnotations: st.AnnotatedTools, WithTitle: st.TitledTools},
		Connected:      []string{}, UnavailableServers: []string{}, Origins: []OriginBrief{},
		LintTotal: len(st.Lint), LintByCode: map[string]int{}, Collisions: len(st.Collisions), Warnings: st.Warnings,
		Hint: `Compact view. Pass detail:"full" (with limit/cursor) for every lint finding and the raw origin records.`,
		Name: st.Name, Visible: st.Visible, Reason: st.Reason, Tool: st.Tool,
	}
	if out.Exclusions == nil {
		out.Exclusions = map[string]int{}
	}
	degraded := []string{}
	for _, origin := range st.Origins {
		brief := OriginBrief{ID: origin.ID, Status: origin.Status, Available: origin.AvailableTools, Cataloged: origin.ToolCount, Degraded: origin.Degraded, OutputSchema: origin.OutputSchemaTools, Annotated: origin.AnnotatedTools, Error: boundText(origin.Error, maxBriefError)}
		out.Origins = append(out.Origins, brief)
		switch {
		case origin.Status != "connected":
			out.UnavailableServers = append(out.UnavailableServers, origin.ID)
		case origin.Degraded:
			degraded = append(degraded, origin.ID)
			out.Connected = append(out.Connected, origin.ID)
		default:
			out.Connected = append(out.Connected, origin.ID)
		}
	}
	for _, finding := range st.Lint {
		out.LintByCode[finding.Code]++
	}
	for i, collision := range st.Collisions {
		if i == 10 {
			break
		}
		out.CollisionNames = append(out.CollisionNames, collision.Name)
	}
	out.Explanation = explainStatus(out, degraded)
	return out
}

func explainStatus(s StatusSummary, degraded []string) string {
	var parts []string
	if s.Complete {
		parts = append(parts, fmt.Sprintf("complete: all %d origins are connected and no tool names collide", len(s.Origins)))
	} else {
		var why []string
		if len(s.UnavailableServers) > 0 {
			why = append(why, fmt.Sprintf("%d origin(s) unavailable (%s): their tools are not in this inventory", len(s.UnavailableServers), strings.Join(s.UnavailableServers, ", ")))
		}
		if len(degraded) > 0 {
			why = append(why, fmt.Sprintf("degraded: %s", strings.Join(degraded, ", ")))
		}
		if s.Collisions > 0 {
			why = append(why, fmt.Sprintf("%d tool name collision(s)", s.Collisions))
		}
		parts = append(parts, "incomplete: "+strings.Join(why, "; ")+"; every other origin is listed in full")
	}
	if s.UnexaminedTools > 0 {
		parts = append(parts, fmt.Sprintf("%d tool schemas were not examined by the conformance lint (advisory, availability is unaffected)", s.UnexaminedTools))
	}
	return strings.Join(parts, ". ")
}

func boundText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

// StatusPage is the full status with its lint findings paged.
type StatusPage struct {
	Status
	LintTotal  int    `json:"lint_total"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Page returns the full status with Lint limited to one page. cursor is the offset
// of the first finding (empty = 0); limit is 1..MaxStatusLint.
func (st Status) Page(limit int, cursor string) (StatusPage, error) {
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 || n > len(st.Lint) {
			return StatusPage{}, fmt.Errorf("cursor is not a valid lint offset")
		}
		offset = n
	}
	if limit <= 0 {
		limit = DefaultStatusLint
	}
	if limit > MaxStatusLint {
		return StatusPage{}, fmt.Errorf("limit must be 1..%d", MaxStatusLint)
	}
	total := len(st.Lint)
	end := min(offset+limit, total)
	page := StatusPage{Status: st, LintTotal: total}
	page.Lint = append([]NameFinding{}, st.Lint[offset:end]...)
	if end < total {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

// Page sizes for the lint findings of a full status.
const (
	DefaultStatusLint = 50
	MaxStatusLint     = 200
)
