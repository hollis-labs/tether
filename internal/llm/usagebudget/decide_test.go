package usagebudget

import (
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	"github.com/hollis-labs/tether/internal/store"
)

func TestBuildFilterDecideCountsDecideSpendPerPurpose(t *testing.T) {
	t.Parallel()

	route := router.Route{Provider: "jev", UsageBudget: router.UsageBudgetPolicy{Level: "provider", Scope: "purpose"}}
	req := llm.Request{
		Operation: llm.OperationDecide,
		Decision:  &llm.DecisionRequest{Purpose: "task-kind"},
	}
	f, err := BuildFilter(req, route)
	if err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
	if f.Operation != "decide" || f.Purpose != "task-kind" || f.Provider != "jev" {
		t.Fatalf("filter = %+v", f)
	}

	req.Decision.Purpose = ""
	if _, err := BuildFilter(req, route); err == nil {
		t.Fatal("purpose scope without a purpose must be rejected")
	}

	// Chat requests keep counting chat spend.
	chat, err := BuildFilter(llm.Request{Operation: llm.OperationChat}, router.Route{})
	if err != nil || chat.Operation != "chat" || chat.Purpose != "" {
		t.Fatalf("chat filter = %+v err=%v", chat, err)
	}
}

func TestEvaluatorRejectsDecideOverPurposeBudget(t *testing.T) {
	t.Parallel()

	maxCost := 0.01
	st := &stubUsageStore{summary: store.AIUsageSummary{EstimatedCostUSD: 0.02}}
	e := Evaluator{Store: st, Now: func() time.Time { return time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) }}
	err := e.Validate(
		llm.Request{Operation: llm.OperationDecide, Decision: &llm.DecisionRequest{Purpose: "backfill"}},
		router.Route{Provider: "jev", UsageBudget: router.UsageBudgetPolicy{Level: "provider", MaxCostUSD: &maxCost, Scope: "purpose", Window: "day"}},
		0,
	)
	if err == nil || !IsRejectionMessage(err.Error()) {
		t.Fatalf("err = %v", err)
	}
	if st.lastFilter.Purpose != "backfill" || st.lastFilter.Operation != "decide" {
		t.Fatalf("filter = %+v", st.lastFilter)
	}
}
