package mcpadapter

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

func TestParseListPageRefusesBadLimitAndCursor(t *testing.T) {
	for _, args := range []map[string]any{
		{"limit": float64(0)},
		{"limit": float64(-3)},
		{"limit": float64(maxListLimit + 1)},
		{"cursor": "!!not-base64!!"},
	} {
		if _, err := parseListPage(args); err == nil {
			t.Errorf("parseListPage(%v): want an error, got none", args)
		}
	}
	p, err := parseListPage(map[string]any{})
	if err != nil || p.Limit != defaultListLimit || p.After != "" || p.Verbose {
		t.Errorf("defaults = %+v, %v", p, err)
	}
}

func TestListPageWalkHasNoGapsOrRepeats(t *testing.T) {
	keys := make([]string, 53)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%03d", i)
	}
	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging did not terminate")
		}
		args := map[string]any{"limit": float64(10)}
		if cursor != "" {
			args["cursor"] = cursor
		}
		p, err := parseListPage(args)
		if err != nil {
			t.Fatal(err)
		}
		start, end, next := p.window(keys)
		seen = append(seen, keys[start:end]...)
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(seen, ",") != strings.Join(keys, ",") {
		t.Fatalf("walk saw %d keys, want %d, in order", len(seen), len(keys))
	}
}

func TestListPageCursorSurvivesRemovedKey(t *testing.T) {
	keys := []string{"a", "b", "c", "d", "e"}
	p, _ := parseListPage(map[string]any{"limit": float64(2)})
	_, _, next := p.window(keys)
	p2, _ := parseListPage(map[string]any{"limit": float64(2), "cursor": next})
	// "b", the cursor key itself, is removed between calls: the next page still starts after it.
	start, end, _ := p2.window([]string{"a", "c", "d", "e"})
	if got := []string{"c", "d"}; fmt.Sprint(got) != fmt.Sprint([]string{"a", "c", "d", "e"}[start:end]) {
		t.Fatalf("page after removed cursor key = %v", []string{"a", "c", "d", "e"}[start:end])
	}
}

func TestBriefAgentDropsLargeFieldsAndReportsTheirSize(t *testing.T) {
	agent := config.Agent{}
	agent.ID = "big"
	agent.SystemPrompt = strings.Repeat("s", 4000)
	agent.AgentPrompt = strings.Repeat("a", 1000)
	agent.Body = strings.Repeat("b", 500)
	agent.Env = map[string]string{"K": "v"}
	b := briefAgent(agent)
	if b.SystemPrompt != "" || b.AgentPrompt != "" || b.Body != "" || b.Env != nil {
		t.Fatalf("brief kept large fields: %+v", b)
	}
	if b.PromptBytes != 5500 || b.ID != "big" {
		t.Fatalf("PromptBytes=%d ID=%q", b.PromptBytes, b.ID)
	}
	if agent.SystemPrompt == "" {
		t.Fatal("briefAgent mutated its input")
	}
}

func TestAgentMatches(t *testing.T) {
	agent := config.Agent{}
	agent.ID = "nanite-ops"
	agent.Name = "Nanite Ops"
	agent.Roles = []string{"sysadmin"}
	for _, tc := range []struct {
		query, role string
		want        bool
	}{
		{"", "", true},
		{"nanite", "", true},
		{"sysadmin", "", true},
		{"zzz", "", false},
		{"", "sysadmin", true},
		{"", "sys", false},
	} {
		if got := agentMatches(agent, tc.query, tc.role); got != tc.want {
			t.Errorf("agentMatches(%q,%q)=%v want %v", tc.query, tc.role, got, tc.want)
		}
	}
}

func TestEventPayloadCap(t *testing.T) {
	small, cut, n := eventPayload("{}", false)
	if small != "{}" || cut || n != 0 {
		t.Fatalf("small payload changed: %q %v %d", small, cut, n)
	}
	big := strings.Repeat("é", maxEventPayloadBytes) // 2 bytes per rune
	got, cut, n := eventPayload(big, false)
	if !cut || n != len(big) || len(got) > maxEventPayloadBytes || strings.ContainsRune(got, '�') {
		t.Fatalf("cut=%v n=%d len=%d", cut, n, len(got))
	}
	if whole, cut, _ := eventPayload(big, true); whole != big || cut {
		t.Fatal("full_payload did not return the payload whole")
	}
}

func TestFitBudgetKeepsAtLeastOneRowAndStopsAtTheBudget(t *testing.T) {
	rows := []string{strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100)}
	if n := fitBudget(rows, 250); n != 2 {
		t.Fatalf("fits %d, want 2", n)
	}
	if n := fitBudget(rows, 10); n != 1 {
		t.Fatalf("a row larger than the budget must still be returned, got %d", n)
	}
	if n := fitBudget(rows, 1<<20); n != 3 {
		t.Fatalf("fits %d, want all 3", n)
	}
	if n := fitBudget([]string{}, 10); n != 0 {
		t.Fatalf("empty = %d", n)
	}
}

// CW-20261024-0053 follow-up: the default 25-agent page measured 19 KB on the hub, over the 16 KB target.
// A brief page now stops at a byte budget and the cursor continues from the last row returned.
func TestAgentListPageStaysUnderTheByteBudgetAndWalksEverything(t *testing.T) {
	cat := &config.Catalog{Agents: map[string]config.Agent{}}
	for i := 0; i < 60; i++ {
		a := config.Agent{}
		a.ID = fmt.Sprintf("agent-%02d", i)
		a.Name = "Agent " + a.ID
		for j := 0; j < 30; j++ { // a long skills list makes each brief row heavy
			a.Skills = append(a.Skills, fmt.Sprintf("a-rather-long-skill-name-%02d", j))
		}
		cat.Agents[a.ID] = a
	}
	adapter := newTestAdapter(t)
	adapter.svc.Catalog = cat
	s := gomcp.NewServer("t", "0")
	adapter.registerTools(s)
	cs := connectInMemory(t, s)

	var seen []string
	cursor := ""
	for pages := 0; pages < 60; pages++ {
		args := map[string]any{}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_catalog_list_agents", Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("list: %v %v", err, textOf(res))
		}
		raw := textOf(res)
		if len(raw) > 16<<10 {
			t.Fatalf("page %d is %d bytes, over the 16 KB target", pages, len(raw))
		}
		body := parseToolJSON(t, res)
		for _, r := range body["agents"].([]any) {
			seen = append(seen, r.(map[string]any)["id"].(string))
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 60 {
		t.Fatalf("walk saw %d agents, want 60 (no gaps, no repeats)", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("order/repeat at %d: %s after %s", i, seen[i], seen[i-1])
		}
	}
}
