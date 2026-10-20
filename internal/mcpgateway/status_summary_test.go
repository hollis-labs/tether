package mcpgateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The 010 case: 12 origins, two failed, ~550 lint findings. The compact answer must stay small and say why.
func bigStatus() Status {
	st := Status{Selection: Selection{Mode: Search}, Exclusions: map[string]int{}, Complete: false, CatalogedTools: 628, EligibleTools: 628, AvailableTools: 628, UnexaminedTools: 480}
	for i := 0; i < 10; i++ {
		st.Origins = append(st.Origins, OriginStatus{ID: fmt.Sprintf("app%d", i), Status: "connected", ToolCount: 60, AvailableTools: 60})
	}
	st.Origins = append(st.Origins,
		OriginStatus{ID: "playwright", Status: "failed", Error: `exec: "npx": executable file not found in $PATH`},
		OriginStatus{ID: "shadcn", Status: "failed", Error: strings.Repeat("x", 1000)})
	for i := 0; i < 547; i++ {
		code := []string{"conformance_unexamined", "origin_prefix", "missing_annotations"}[i%3]
		st.Lint = append(st.Lint, NameFinding{Origin: "app1", Name: fmt.Sprintf("tool_%d", i), Code: code, Message: strings.Repeat("m", 120)})
	}
	return st
}

func TestStatusSummaryIsCompactAndExplainsIncomplete(t *testing.T) {
	st := bigStatus()
	full, _ := json.Marshal(st)
	sum := st.Summary()
	raw, _ := json.Marshal(sum)
	if len(raw) > 4096 {
		t.Fatalf("summary is %d bytes (full %d), want <= 4096", len(raw), len(full))
	}
	if len(full) < 50000 {
		t.Fatalf("test fixture too small to prove anything: %d", len(full))
	}
	if sum.Complete || len(sum.Connected) != 10 || strings.Join(sum.UnavailableServers, ",") != "playwright,shadcn" {
		t.Fatalf("connected=%v unavailable=%v complete=%v", sum.Connected, sum.UnavailableServers, sum.Complete)
	}
	if sum.LintTotal != 547 || sum.LintByCode["origin_prefix"] == 0 || sum.UnexaminedTools != 480 {
		t.Fatalf("lint totals wrong: %+v", sum.LintByCode)
	}
	for _, want := range []string{"incomplete", "playwright, shadcn", "not in this inventory", "480 tool schemas"} {
		if !strings.Contains(sum.Explanation, want) {
			t.Errorf("explanation %q lacks %q", sum.Explanation, want)
		}
	}
	for _, o := range sum.Origins {
		if len(o.Error) > maxBriefError+3 {
			t.Errorf("origin %s error not bounded: %d", o.ID, len(o.Error))
		}
	}
}

func TestStatusSummaryCompleteSaysSo(t *testing.T) {
	st := Status{Complete: true, Exclusions: map[string]int{}, Origins: []OriginStatus{{ID: "a", Status: "connected"}}}
	if s := st.Summary(); !s.Complete || !strings.HasPrefix(s.Explanation, "complete: all 1 origins") {
		t.Fatalf("%+v", s)
	}
}

func TestStatusPagePagesLintOnRequest(t *testing.T) {
	st := bigStatus()
	seen, cursor := 0, ""
	for pages := 0; ; pages++ {
		page, err := st.Page(200, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if page.LintTotal != 547 || len(page.Lint) > 200 {
			t.Fatalf("total=%d page=%d", page.LintTotal, len(page.Lint))
		}
		seen += len(page.Lint)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 5 {
			t.Fatal("paging does not end")
		}
	}
	if seen != 547 {
		t.Fatalf("paged %d of 547 findings", seen)
	}
	for _, bad := range []struct {
		limit  int
		cursor string
	}{{201, ""}, {10, "abc"}, {10, "-1"}, {10, "9999"}} {
		if _, err := st.Page(bad.limit, bad.cursor); err == nil {
			t.Errorf("accepted limit=%d cursor=%q", bad.limit, bad.cursor)
		}
	}
}
