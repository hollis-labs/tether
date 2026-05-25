package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/llm/observability"
)

func TestRecordAndQueryAIEvents(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ai.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	now := time.Now().UTC()
	evs := []observability.AuditEvent{
		{EventType: "chat", Operation: "chat", Provider: "anthropic-work", Model: "claude-sonnet-4-5", Success: true, Timestamp: now.Add(-2 * time.Second)},
		{EventType: "route_preview", Operation: "chat", Provider: "anthropic-work", Model: "claude-sonnet-4-5", Success: false, Error: "no route", Timestamp: now.Add(-1 * time.Second)},
		{EventType: "budget_rejection", Operation: "chat", Provider: "anthropic-work", Model: "claude-sonnet-4-5", Success: false, Error: "usage budget 1.000000 USD/month exceeded: spent 1.200000 USD", Timestamp: now},
	}
	for _, ev := range evs {
		if err := s.RecordAIAuditEvent(ev); err != nil {
			t.Fatalf("RecordAIAuditEvent: %v", err)
		}
	}

	rows, err := s.QueryAIEvents(AIEventFilter{Limit: 10})
	if err != nil {
		t.Fatalf("QueryAIEvents: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows len = %d, want 3", len(rows))
	}

	errOnly, err := s.QueryAIEvents(AIEventFilter{ErrorsOnly: true, Limit: 10})
	if err != nil {
		t.Fatalf("QueryAIEvents errors only: %v", err)
	}
	if len(errOnly) != 2 {
		t.Fatalf("error rows = %+v", errOnly)
	}
	budgetRows, err := s.QueryAIEvents(AIEventFilter{EventType: "budget_rejection", Limit: 10})
	if err != nil {
		t.Fatalf("QueryAIEvents budget_rejection: %v", err)
	}
	if len(budgetRows) != 1 || budgetRows[0].Provider != "anthropic-work" {
		t.Fatalf("budget rows = %+v", budgetRows)
	}
}

func TestQueryAIUsageSummary(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	now := time.Now().UTC()
	evs := []observability.AuditEvent{
		{
			EventType:        "chat",
			Operation:        "chat",
			Provider:         "anthropic-work",
			Model:            "claude-sonnet-4-5",
			Success:          true,
			LatencyMs:        200,
			InputTokens:      100,
			OutputTokens:     40,
			ReasoningTokens:  8,
			EstimatedCostUSD: 0.01,
			Timestamp:        now.Add(-2 * time.Second),
		},
		{
			EventType:        "chat",
			Operation:        "chat",
			Provider:         "anthropic-work",
			Model:            "claude-sonnet-4-5",
			Success:          false,
			Error:            "provider timeout",
			LatencyMs:        300,
			InputTokens:      90,
			OutputTokens:     0,
			EstimatedCostUSD: 0.00,
			Timestamp:        now.Add(-1 * time.Second),
		},
		{
			EventType:        "route_preview",
			Operation:        "chat",
			Provider:         "anthropic-work",
			Model:            "claude-sonnet-4-5",
			Success:          true,
			EstimatedCostUSD: 99,
			Timestamp:        now,
		},
		{
			EventType:        "budget_rejection",
			Operation:        "chat",
			Provider:         "anthropic-work",
			Model:            "claude-sonnet-4-5",
			Success:          false,
			Error:            "usage budget 1.000000 USD/month exceeded: spent 1.200000 USD",
			EstimatedCostUSD: 42,
			Timestamp:        now.Add(500 * time.Millisecond),
		},
	}
	for _, ev := range evs {
		if err := s.RecordAIAuditEvent(ev); err != nil {
			t.Fatalf("RecordAIAuditEvent: %v", err)
		}
	}

	summary, err := s.QueryAIUsageSummary(AIUsageFilter{Provider: "anthropic-work"})
	if err != nil {
		t.Fatalf("QueryAIUsageSummary: %v", err)
	}
	if summary.Requests != 2 || summary.Successes != 1 || summary.Errors != 1 {
		t.Fatalf("summary counts = %+v", summary)
	}
	if summary.InputTokens != 190 || summary.OutputTokens != 40 || summary.ReasoningTokens != 8 {
		t.Fatalf("summary usage = %+v", summary)
	}
	if len(summary.ByProvider) != 1 || summary.ByProvider[0].Key != "anthropic-work" {
		t.Fatalf("provider breakdown = %+v", summary.ByProvider)
	}
	if len(summary.ByOperation) != 1 || summary.ByOperation[0].Key != "chat" {
		t.Fatalf("operation breakdown = %+v", summary.ByOperation)
	}
}
