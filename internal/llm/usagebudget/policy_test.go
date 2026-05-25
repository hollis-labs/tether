package usagebudget

import (
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	"github.com/hollis-labs/tether/internal/store"
)

func TestEvaluatorRejectsProjectedBudgetOverflow(t *testing.T) {
	t.Parallel()

	maxCost := 1.00
	e := Evaluator{
		Store: &stubUsageStore{summary: store.AIUsageSummary{EstimatedCostUSD: 0.95}},
		Now:   func() time.Time { return time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC) },
	}

	err := e.Validate(
		llm.Request{Operation: llm.OperationChat, CallerID: "agent-1"},
		router.Route{
			Provider: "anthropic-work",
			Model:    "claude-sonnet-4-5",
			UsageBudget: router.UsageBudgetPolicy{
				Level:      "provider",
				MaxCostUSD: &maxCost,
				Window:     "month",
				Scope:      "caller",
			},
		},
		0.10,
	)
	if err == nil || err.Error() != "usage budget 1.000000 USD/month would be exceeded: spent 0.950000 USD + estimated 0.100000 USD" {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestEvaluatorRequiresCallerIDForCallerScope(t *testing.T) {
	t.Parallel()

	maxCost := 1.00
	e := Evaluator{Store: &stubUsageStore{}}
	err := e.Validate(
		llm.Request{Operation: llm.OperationChat},
		router.Route{UsageBudget: router.UsageBudgetPolicy{
			Level:      "global",
			MaxCostUSD: &maxCost,
			Scope:      "caller",
		}},
		0,
	)
	if err == nil || err.Error() != "usage budget scope caller requires caller_id" {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestEvaluatorUsesRouteLevelProviderAndModelFilters(t *testing.T) {
	t.Parallel()

	maxCost := 1.00
	store := &stubUsageStore{summary: store.AIUsageSummary{EstimatedCostUSD: 0.20}}
	e := Evaluator{
		Store: store,
		Now:   func() time.Time { return time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC) },
	}

	err := e.Validate(
		llm.Request{Operation: llm.OperationChat, SessionID: "sess-1"},
		router.Route{
			Provider: "openai-work",
			Model:    "gpt-5",
			UsageBudget: router.UsageBudgetPolicy{
				Level:      "route",
				MaxCostUSD: &maxCost,
				Window:     "day",
				Scope:      "session",
			},
		},
		0.05,
	)
	if err != nil {
		t.Fatalf("Validate error = %v", err)
	}
	if store.lastFilter.Provider != "openai-work" || store.lastFilter.Model != "gpt-5" || store.lastFilter.SessionID != "sess-1" {
		t.Fatalf("filter = %+v", store.lastFilter)
	}
	if got := store.lastFilter.Since; !got.Equal(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("since = %s", got)
	}
}

type stubUsageStore struct {
	summary    store.AIUsageSummary
	lastFilter store.AIUsageFilter
}

func (s *stubUsageStore) QueryAIUsageSummary(f store.AIUsageFilter) (store.AIUsageSummary, error) {
	s.lastFilter = f
	return s.summary, nil
}
