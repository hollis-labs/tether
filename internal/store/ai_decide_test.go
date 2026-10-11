package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/llm/observability"
)

func TestAIDecideEventsKeepPurposeAndBudgetPerPurpose(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ai.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().UTC()
	evs := []observability.AuditEvent{
		{EventType: "decide", Operation: "decide", Purpose: "kind", Provider: "jev", Model: "jev-1.13.0", Success: true, EstimatedCostUSD: 0.01, InputTokens: 100, Timestamp: now},
		{EventType: "decide", Operation: "decide", Purpose: "kind", Provider: "jev", Model: "jev-1.13.0", Success: true, EstimatedCostUSD: 0.02, InputTokens: 200, Timestamp: now},
		{EventType: "decide", Operation: "decide", Purpose: "tags", Provider: "jev", Model: "jev-1.13.0", Success: true, EstimatedCostUSD: 0.5, Timestamp: now},
		{EventType: "chat", Operation: "chat", Provider: "anthropic-work", Model: "m", Success: true, EstimatedCostUSD: 9, Timestamp: now},
	}
	for _, ev := range evs {
		if err := s.RecordAIAuditEvent(ev); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	kind, err := s.QueryAIUsageSummary(AIUsageFilter{Operation: "decide", Purpose: "kind"})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if kind.Requests != 2 || kind.EstimatedCostUSD < 0.0299 || kind.EstimatedCostUSD > 0.0301 || kind.InputTokens != 300 {
		t.Fatalf("kind summary = %+v", kind)
	}
	all, err := s.QueryAIUsageSummary(AIUsageFilter{Operation: "decide"})
	if err != nil || all.Requests != 3 {
		t.Fatalf("decide summary must exclude chat: %+v err=%v", all, err)
	}
	chat, err := s.QueryAIUsageSummary(AIUsageFilter{Operation: "chat"})
	if err != nil || chat.Requests != 1 || chat.EstimatedCostUSD != 9 {
		t.Fatalf("chat summary must exclude decide: %+v err=%v", chat, err)
	}

	rows, err := s.QueryAIEvents(AIEventFilter{EventType: "decide", Limit: 10})
	if err != nil || len(rows) != 3 || rows[0].Purpose != "kind" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
