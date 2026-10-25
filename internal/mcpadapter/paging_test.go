package mcpadapter

import (
	"fmt"
	"strings"
	"testing"

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
