package mcpgateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBriefDescription(t *testing.T) {
	long := strings.Repeat("word ", 80) // 400 chars, no sentence end
	for _, tc := range []struct {
		name, in, want string
		clean          bool
	}{
		{"empty", "", "", true},
		{"one sentence", "Read widgets from the store.", "Read widgets from the store.", true},
		{"first of several", "Send a message. Requires message.write scope. More text.", "Send a message.", true},
		{"stops at a blank line", "Render a card\nacross two lines\n\nWhen to use: always.", "Render a card across two lines", true},
		{"abbreviation is not a sentence end", "Read records, e.g. tasks and plans. Then more.", "Read records, e.g. tasks and plans.", true},
		{"version number is not a sentence end", "Calls the v0.5 API and returns JSON. Extra.", "Calls the v0.5 API and returns JSON.", true},
		{"question mark ends it", "Is it up? Check and report.", "Is it up?", true},
		{"whitespace collapses", "  Read   the\tstore.  ", "Read the store.", true},
		{"multibyte stays valid", "界界界。 not a split. 次", "界界界。 not a split.", true},
		{"too long is cut at a word with an ellipsis", long, "", false},
	} {
		got, clean := BriefDescription(tc.in)
		if clean != tc.clean {
			t.Errorf("%s: clean = %v want %v (%q)", tc.name, clean, tc.clean, got)
		}
		if tc.want != "" && got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
		if utf8.RuneCountInString(got) > BriefDescriptionMax {
			t.Errorf("%s: %d runes exceeds %d", tc.name, utf8.RuneCountInString(got), BriefDescriptionMax)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: invalid UTF-8 %q", tc.name, got)
		}
	}
	got, _ := BriefDescription(long)
	if !strings.HasSuffix(got, "…") || strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Errorf("truncation should end with a bare ellipsis, got %q", got)
	}
}

func TestRequiredArgs(t *testing.T) {
	if got := requiredArgs(map[string]any{"type": "object", "required": []any{"from", "to", "kind"}}); strings.Join(got, ",") != "from,to,kind" {
		t.Errorf("[]any: %v", got)
	}
	if got := requiredArgs(map[string]any{"required": []string{"a"}}); strings.Join(got, ",") != "a" {
		t.Errorf("[]string: %v", got)
	}
	// A typed struct goes through JSON.
	type typed struct {
		Required []string `json:"required"`
	}
	if got := requiredArgs(typed{Required: []string{"x", "y"}}); strings.Join(got, ",") != "x,y" {
		t.Errorf("typed: %v", got)
	}
	if requiredArgs(nil) != nil || requiredArgs(map[string]any{"type": "object"}) != nil || requiredArgs(42) != nil {
		t.Error("unreadable or absent required must yield nil")
	}
	many := make([]any, 30)
	for i := range many {
		many[i] = fmt.Sprintf("arg%d", i)
	}
	if got := requiredArgs(map[string]any{"required": many}); len(got) != maxRequiredArgs {
		t.Errorf("bounded: %d", len(got))
	}
}

// dietService builds n tools across two origins whose full descriptions are far
// longer than a summary and whose schemas are large, so the cap has work to do.
func dietService(n int, maxBytes int) *Service {
	desc := "Read widgets from the store. " + strings.Repeat("Long explanatory prose about modes, defaults and caveats. ", 40)
	props := map[string]any{}
	for i := 0; i < 20; i++ {
		props[fmt.Sprintf("field_%d", i)] = map[string]any{"type": "string", "description": strings.Repeat("describes the field ", 8)}
	}
	var entries []Entry
	origins := []OriginStatus{{ID: "alpha", Status: "connected"}, {ID: "beta", Status: "connected"}}
	for i := 0; i < n; i++ {
		origin := origins[i%2].ID
		entries = append(entries, Entry{Origin: origin, Tool: &mcpsdk.Tool{
			Name: fmt.Sprintf("%s_read_widgets_%02d", origin, i), Title: "Widgets", Description: desc,
			Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
			InputSchema: map[string]any{"type": "object", "properties": props, "required": []any{"field_0", "field_1"}},
		}})
	}
	return &Service{
		Snapshot:       func() Snapshot { return Snapshot{Entries: entries, Origins: origins} },
		Score:          KeywordScore,
		MaxResultBytes: maxBytes,
	}
}

func encodedLen(t *testing.T, v any) int {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

func TestSearchDetailLevels(t *testing.T) {
	svc := dietService(12, 1<<20) // the byte cap is tested separately; keep it out of the way here
	names, err := svc.Search(Request{Query: "read widgets", Detail: "names"})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := svc.Search(Request{Query: "read widgets"}) // default detail
	if err != nil {
		t.Fatal(err)
	}
	schema, err := svc.Search(Request{Query: "read widgets", Detail: "schema"})
	if err != nil {
		t.Fatal(err)
	}
	if names.Returned == 0 || names.Returned != summary.Returned || summary.Returned != schema.Returned {
		t.Fatalf("detail changed which tools match: %d %d %d", names.Returned, summary.Returned, schema.Returned)
	}
	n, s, f := encodedLen(t, names), encodedLen(t, summary), encodedLen(t, schema)
	if n >= s || s >= f {
		t.Fatalf("sizes must grow with detail: names=%d summary=%d schema=%d", n, s, f)
	}
	if s*3 > f {
		t.Errorf("summary (%d B) should be well under a third of schema detail (%d B)", s, f)
	}
	for _, item := range names.Items {
		if item.Description != "" || item.InputSchema != nil || item.Annotations != nil || item.RequiredArgs != nil {
			t.Fatalf("names detail leaked more than name/origin: %+v", item)
		}
		if item.Name == "" || item.Origin == "" {
			t.Fatalf("names detail lacks name or origin: %+v", item)
		}
	}
	for _, item := range summary.Items {
		if item.Description != "Read widgets from the store." {
			t.Fatalf("summary description should be the first sentence, got %q", item.Description)
		}
		if item.InputSchema != nil || item.OutputSchema != nil || item.Meta != nil || len(item.Icons) != 0 {
			t.Fatalf("summary carried schema, meta or icons: %+v", item)
		}
		if item.Annotations == nil || strings.Join(item.RequiredArgs, ",") != "field_0,field_1" || item.Title != "Widgets" {
			t.Fatalf("summary lost annotations, required args or title: %+v", item)
		}
	}
	for _, item := range schema.Items {
		if !strings.Contains(item.Description, "Long explanatory prose") || item.InputSchema == nil {
			t.Fatalf("schema detail must keep the full description and schema: %+v", item)
		}
	}
	if _, err := svc.Search(Request{Query: "x", Detail: "everything"}); err == nil || !strings.Contains(err.Error(), "names, summary or schema") {
		t.Fatalf("invalid detail must name the valid levels, got %v", err)
	}
	// Items stay in rank order; origins are summarized separately.
	if got := summary.OriginCounts["alpha"] + summary.OriginCounts["beta"]; got != summary.Returned {
		t.Fatalf("origin_counts %v do not add up to %d returned", summary.OriginCounts, summary.Returned)
	}
}

func TestListNamesAreCutAtItemBoundariesAndOmittedAreReported(t *testing.T) {
	svc := dietService(20, 6000) // each full item is ~3KB: only a couple fit
	var names []string
	for i := 0; i < 20; i++ {
		origin := []string{"alpha", "beta"}[i%2]
		names = append(names, fmt.Sprintf("%s_read_widgets_%02d", origin, i))
	}
	got, err := svc.List(Request{Names: names})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.TruncationReason != "bytes" || got.NextCursor != "" {
		t.Fatalf("want a bytes cut without a cursor (names hydration has none): %+v", got)
	}
	if got.Returned == 0 || got.Returned >= len(names) {
		t.Fatalf("returned %d of %d", got.Returned, len(names))
	}
	if got.Returned+len(got.OmittedNames) != len(names) {
		t.Fatalf("returned %d + omitted %d != requested %d", got.Returned, len(got.OmittedNames), len(names))
	}
	// Every returned item is whole, in request order, and omitted names are the tail.
	for i, item := range got.Items {
		if item.Name != names[i] || item.InputSchema == nil {
			t.Fatalf("item %d is not a whole requested tool: %+v", i, item)
		}
	}
	if got.OmittedNames[0] != names[got.Returned] {
		t.Fatalf("omitted names must start where the cut happened: %v", got.OmittedNames)
	}
	// Requesting the omitted names again makes progress and finally completes.
	rest, seen := got.OmittedNames, got.Returned
	for rounds := 0; len(rest) > 0 && rounds < 30; rounds++ {
		next, err := svc.List(Request{Names: rest})
		if err != nil {
			t.Fatal(err)
		}
		if next.Returned == 0 {
			t.Fatal("re-request returned nothing: livelock")
		}
		seen += next.Returned
		rest = next.OmittedNames
	}
	if seen != len(names) || len(rest) != 0 {
		t.Fatalf("hydrated %d of %d, still omitted %v", seen, len(names), rest)
	}
}

func TestOversizedFirstItemIsStillReturned(t *testing.T) {
	svc := dietService(3, 50) // far smaller than any single item
	got, err := svc.List(Request{Names: []string{"alpha_read_widgets_00", "beta_read_widgets_01"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Returned != 1 || got.Items[0].Name != "alpha_read_widgets_00" || !got.Truncated || len(got.OmittedNames) != 1 {
		t.Fatalf("the first item must always be returned so a caller can make progress: %+v", got)
	}
}

func TestEnumerationCursorResumesAfterAByteCutWithoutGapsOrDuplicates(t *testing.T) {
	svc := dietService(14, 7000)
	seen := map[string]int{}
	cursor, pages, reasons := "", 0, map[string]int{}
	for {
		page, err := svc.Search(Request{Query: "read widgets", Detail: "schema", Limit: 50, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if page.Returned == 0 {
			t.Fatalf("page %d empty", pages)
		}
		if page.TotalMatches != 14 {
			t.Fatalf("total_matches %d", page.TotalMatches)
		}
		for _, item := range page.Items {
			seen[item.Name]++
		}
		if !page.Truncated {
			break
		}
		reasons[page.TruncationReason]++
		if page.NextCursor == "" {
			t.Fatalf("truncated page %d without a cursor", pages)
		}
		cursor = page.NextCursor
		if pages > 20 {
			t.Fatal("did not terminate")
		}
	}
	if len(seen) != 14 {
		t.Fatalf("saw %d of 14 distinct tools: gap", len(seen))
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("%s returned %d times: duplicate", name, count)
		}
	}
	if pages < 3 || reasons["bytes"] == 0 {
		t.Fatalf("expected byte-cut pages, got %d pages, reasons %v", pages, reasons)
	}
}

func TestPageSizeLimitStillReportsLimitAsTheReason(t *testing.T) {
	svc := dietService(10, 0)
	page, err := svc.Search(Request{Query: "read widgets", Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if page.Returned != 4 || !page.Truncated || page.TruncationReason != "limit" || page.NextCursor == "" {
		t.Fatalf("%+v", page)
	}
}

func TestDefaultSummarySearchStaysSmall(t *testing.T) {
	svc := dietService(60, 0)
	page, err := svc.Search(Request{Query: "read widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if size := encodedLen(t, page); size > 3500 {
		t.Fatalf("a default 10-result summary page is %d bytes; target is a few KB", size)
	}
}

func TestDefaultCapCutsASchemaPageButNotASummaryPage(t *testing.T) {
	svc := dietService(40, 0) // default cap
	schema, err := svc.Search(Request{Query: "read widgets", Detail: "schema", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !schema.Truncated || schema.TruncationReason != "bytes" || schema.Returned >= 20 {
		t.Fatalf("a 20-item schema page should be cut by the default cap: returned=%d reason=%q", schema.Returned, schema.TruncationReason)
	}
	summary, err := svc.Search(Request{Query: "read widgets", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Returned != 20 || summary.TruncationReason != "limit" {
		t.Fatalf("a 20-item summary page should fit: returned=%d reason=%q", summary.Returned, summary.TruncationReason)
	}
}
