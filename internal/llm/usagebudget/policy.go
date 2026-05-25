package usagebudget

import (
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	"github.com/hollis-labs/tether/internal/store"
)

// UsageStore is the historical usage query surface needed for durable budget
// enforcement.
type UsageStore interface {
	QueryAIUsageSummary(f store.AIUsageFilter) (store.AIUsageSummary, error)
}

// Evaluator rejects routes whose durable usage budget would already be
// exceeded by the historical spend captured in ai_events.
type Evaluator struct {
	Store UsageStore
	Now   func() time.Time
}

const rejectionPrefix = "usage budget "

// Validate implements router.PolicyEvaluator.
func (e Evaluator) Validate(req llm.Request, route router.Route, estimatedCostUSD float64) error {
	budget := route.UsageBudget
	if e.Store == nil || budget.MaxCostUSD == nil {
		return nil
	}

	filter, err := BuildFilter(req, route)
	if err != nil {
		return err
	}
	filter.Since = budgetWindowStart(now(e.Now), budget.Window)

	summary, err := e.Store.QueryAIUsageSummary(filter)
	if err != nil {
		return fmt.Errorf("query usage budget: %w", err)
	}
	spent := summary.EstimatedCostUSD
	limit := *budget.MaxCostUSD
	window := normalizedBudgetWindow(budget.Window)

	if spent >= limit {
		return fmt.Errorf(
			rejectionPrefix+"%.6f USD/%s exceeded: spent %.6f USD",
			limit,
			window,
			spent,
		)
	}
	if estimatedCostUSD > 0 && spent+estimatedCostUSD > limit {
		return fmt.Errorf(
			rejectionPrefix+"%.6f USD/%s would be exceeded: spent %.6f USD + estimated %.6f USD",
			limit,
			window,
			spent,
			estimatedCostUSD,
		)
	}
	return nil
}

// BuildFilter resolves the ai_events usage query filter implied by one route's
// effective usage budget and one request's correlation metadata.
func BuildFilter(req llm.Request, route router.Route) (store.AIUsageFilter, error) {
	filter := store.AIUsageFilter{Operation: string(llm.OperationChat)}
	switch route.UsageBudget.Level {
	case "", "global":
	case "provider":
		filter.Provider = route.Provider
	case "route":
		filter.Provider = route.Provider
		filter.Model = route.Model
	default:
		return store.AIUsageFilter{}, fmt.Errorf("unsupported usage budget level %q", route.UsageBudget.Level)
	}

	switch normalizedBudgetScope(route.UsageBudget.Scope) {
	case "total":
	case "caller":
		if req.CallerID == "" {
			return store.AIUsageFilter{}, fmt.Errorf("usage budget scope caller requires caller_id")
		}
		filter.CallerID = req.CallerID
	case "session":
		if req.SessionID == "" {
			return store.AIUsageFilter{}, fmt.Errorf("usage budget scope session requires session_id")
		}
		filter.SessionID = req.SessionID
	default:
		return store.AIUsageFilter{}, fmt.Errorf("unsupported usage budget scope %q", route.UsageBudget.Scope)
	}
	return filter, nil
}

func budgetWindowStart(now time.Time, window string) time.Time {
	now = now.UTC()
	switch normalizedBudgetWindow(window) {
	case "day":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	default:
		y, m, _ := now.Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	}
}

func normalizedBudgetWindow(window string) string {
	if window == "" {
		return "month"
	}
	return window
}

func normalizedBudgetScope(scope string) string {
	if scope == "" {
		return "total"
	}
	return scope
}

func now(fn func() time.Time) time.Time {
	if fn != nil {
		return fn()
	}
	return time.Now()
}

// IsRejectionMessage reports whether one planner/audit error string is a
// durable usage-budget rejection.
func IsRejectionMessage(msg string) bool {
	return strings.HasPrefix(strings.TrimSpace(msg), rejectionPrefix)
}
