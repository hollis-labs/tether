package router

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/llm-core/modelsdev"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/modelcatalog"
)

func TestPlannerSelectsPinnedProviderAndModel(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{ToolCall: true}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{Version: "v1"})

	plan, err := planner.Plan(llm.Request{
		Operation:    llm.OperationChat,
		ProviderHint: "openai",
		ModelHint:    "gpt-4o-mini",
		Tools:        []llm.ToolDefinition{{Name: "search"}},
	})
	if err != nil {
		t.Fatalf("Plan returned err: %v", err)
	}
	if plan.Provider != "openai" || plan.Model != "gpt-4o-mini" {
		t.Fatalf("plan = %+v", plan)
	}
	if !reflect.DeepEqual(plan.RouteDecision(), llm.RouteDecision{
		Provider:      "openai",
		Model:         "gpt-4o-mini",
		Reasons:       []string{"matched provider hint", "matched model hint", "supports tool calling"},
		PolicyVersion: "v1",
	}) {
		t.Fatalf("RouteDecision = %+v", plan.RouteDecision())
	}
}

func TestPlannerUsesCatalogProviderForConfiguredProviderID(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-5-mini", 400000, 128000, 0.25, 2, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v1",
		Routes: []Route{{
			Provider:        "openai-work",
			CatalogProvider: "openai",
			Model:           "gpt-5-mini",
		}},
	})

	plan, err := planner.Plan(llm.Request{Operation: llm.OperationChat})
	if err != nil {
		t.Fatalf("Plan returned err: %v", err)
	}
	if plan.Provider != "openai-work" || plan.Model != "gpt-5-mini" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlannerFallsBackWhenFirstRouteLacksCapabilities(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
			modelRef("anthropic", "claude-sonnet-4-5", 200000, 16000, 3, 15, modelsdev.Capabilities{ToolCall: true, Reasoning: true}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v2",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini"},
			{Provider: "anthropic", Model: "claude-sonnet-4-5"},
		},
	})

	plan, err := planner.Plan(llm.Request{
		Operation: llm.OperationChat,
		Intent:    "reasoning",
		Tools:     []llm.ToolDefinition{{Name: "shell"}},
	})
	if err != nil {
		t.Fatalf("Plan returned err: %v", err)
	}
	if plan.Provider != "anthropic" || plan.Model != "claude-sonnet-4-5" {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(strings.Join(plan.Reasons, " | "), "selected after ordered fallback") {
		t.Fatalf("Reasons = %v, want fallback marker", plan.Reasons)
	}
}

func TestPlannerFallsBackOnCostBudget(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("anthropic", "claude-sonnet-4-5", 200000, 16000, 3, 15, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v3",
		Routes: []Route{
			{Provider: "anthropic", Model: "claude-sonnet-4-5"},
			{Provider: "openai", Model: "gpt-4o-mini"},
		},
	})

	plan, err := planner.Plan(llm.Request{
		Operation:       llm.OperationChat,
		MaxInputTokens:  1000,
		MaxOutputTokens: 500,
		CostBudgetUSD:   0.01,
	})
	if err != nil {
		t.Fatalf("Plan returned err: %v", err)
	}
	if plan.Provider != "openai" {
		t.Fatalf("Provider = %q, want openai", plan.Provider)
	}
	if plan.EstimatedCostUSD <= 0 || plan.EstimatedCostUSD > 0.01 {
		t.Fatalf("EstimatedCostUSD = %f", plan.EstimatedCostUSD)
	}
}

func TestPlannerRejectsUnsupportedImageInput(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{Attachment: true}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Routes: []Route{{Provider: "openai", Model: "gpt-4o-mini"}},
	})

	_, err := planner.Plan(llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{{
			Role: "user",
			Parts: []llm.ContentPart{
				{Type: "text", Text: "describe this"},
				{Type: "image", URL: "file:///tmp/demo.png"},
			},
		}},
	})
	if err == nil {
		t.Fatal("Plan error = nil, want non-nil")
	}
	if !errors.Is(err, ErrNoRouteMatch) {
		t.Fatalf("Plan error = %v, want ErrNoRouteMatch", err)
	}
	if !strings.Contains(err.Error(), `input modality "image" unsupported`) {
		t.Fatalf("Plan error = %v", err)
	}
}

func TestPlannerReturnsNoCandidatesForUnknownHint(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{})

	_, err := planner.Plan(llm.Request{
		Operation:    llm.OperationChat,
		ProviderHint: "missing-provider",
	})
	if !errors.Is(err, ErrNoRouteMatch) && !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("Plan error = %v, want routing error", err)
	}
}

func TestPlannerPrefersModeMatchedRoute(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
			modelRef("anthropic", "claude-sonnet-4-5", 200000, 16000, 3, 15, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v4",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini", Mode: "summarize"},
			{Provider: "anthropic", Model: "claude-sonnet-4-5"},
		},
	})

	plan, err := planner.Plan(llm.Request{
		Operation: llm.OperationChat,
		Mode:      "summarize",
	})
	if err != nil {
		t.Fatalf("Plan returned err: %v", err)
	}
	if plan.Provider != "openai" || plan.Model != "gpt-4o-mini" {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(strings.Join(plan.Reasons, " | "), `matched route mode "summarize"`) {
		t.Fatalf("Reasons = %v", plan.Reasons)
	}
}

func TestPlannerPrefersReasoningMatchedRoute(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{Reasoning: true}, []string{"text"}, []string{"text"}),
			modelRef("anthropic", "claude-sonnet-4-5", 200000, 16000, 3, 15, modelsdev.Capabilities{Reasoning: true}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v5",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini"},
			{Provider: "anthropic", Model: "claude-sonnet-4-5", RequiresReasoning: true},
		},
	})

	plan, err := planner.Plan(llm.Request{
		Operation: llm.OperationChat,
		Intent:    "reasoning",
	})
	if err != nil {
		t.Fatalf("Plan returned err: %v", err)
	}
	if plan.Provider != "anthropic" || plan.Model != "claude-sonnet-4-5" {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(strings.Join(plan.Reasons, " | "), "matched route reasoning requirement") {
		t.Fatalf("Reasons = %v", plan.Reasons)
	}
}

func TestPlannerReturnsNoCandidatesWhenExplicitRoutesDoNotMatch(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v6",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini", Mode: "summarize"},
		},
	})

	_, err := planner.Plan(llm.Request{
		Operation: llm.OperationChat,
		Mode:      "chat",
	})
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("Plan error = %v, want ErrNoCandidates", err)
	}
}

func TestPlannerExplainIncludesWinnerAndFailures(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
			modelRef("anthropic", "claude-sonnet-4-5", 200000, 16000, 3, 15, modelsdev.Capabilities{Reasoning: true}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v7",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini", RequiresReasoning: true},
			{Provider: "anthropic", Model: "claude-sonnet-4-5", RequiresReasoning: true},
		},
	})

	explanation := planner.Explain(llm.Request{
		Operation: llm.OperationChat,
		Intent:    "reasoning",
	})
	if explanation.Winner == nil || explanation.Winner.Provider != "anthropic" {
		t.Fatalf("winner = %+v", explanation.Winner)
	}
	if len(explanation.Candidates) != 2 {
		t.Fatalf("candidates = %+v", explanation.Candidates)
	}
	if explanation.Candidates[0].Error == "" || !explanation.Candidates[1].Selected {
		t.Fatalf("candidates = %+v", explanation.Candidates)
	}
}

func TestPlannerExplainIncludesExclusionReasons(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	planner := New(catalog, Policy{
		Version: "v8",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini", Mode: "summarize"},
		},
	})

	explanation := planner.Explain(llm.Request{
		Operation: llm.OperationChat,
		Mode:      "chat",
	})
	if explanation.Error == "" || len(explanation.Candidates) != 1 {
		t.Fatalf("explanation = %+v", explanation)
	}
	if !strings.Contains(strings.Join(explanation.Candidates[0].Reasons, " | "), `route mode "summarize" did not match request mode "chat"`) {
		t.Fatalf("candidate reasons = %v", explanation.Candidates[0].Reasons)
	}
}

func TestPlannerExplainShowsRoutePolicyRejection(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{ToolCall: true}, []string{"text"}, []string{"text"}),
		},
	}
	disallowTools := false
	planner := New(catalog, Policy{
		Version: "v9",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini", AllowTools: &disallowTools},
		},
	})

	_, err := planner.Plan(llm.Request{
		Operation: llm.OperationChat,
		Tools:     []llm.ToolDefinition{{Name: "search"}},
	})
	if !errors.Is(err, ErrNoRouteMatch) {
		t.Fatalf("Plan error = %v, want ErrNoRouteMatch", err)
	}

	explanation := planner.Explain(llm.Request{
		Operation: llm.OperationChat,
		Tools:     []llm.ToolDefinition{{Name: "search"}},
	})
	if len(explanation.Candidates) != 1 || explanation.Candidates[0].Error != "route policy disallows tools" {
		t.Fatalf("explanation = %+v", explanation)
	}
}

func TestPlannerExplainShowsRouteBudgetPolicyRejection(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	maxOutput := 256
	maxCost := 0.05
	planner := New(catalog, Policy{
		Version: "v10",
		Routes: []Route{
			{Provider: "openai", Model: "gpt-4o-mini", MaxOutputTokens: &maxOutput, MaxCostUSD: &maxCost},
		},
	})

	explanation := planner.Explain(llm.Request{
		Operation:       llm.OperationChat,
		MaxOutputTokens: 512,
		CostBudgetUSD:   0.10,
	})
	if len(explanation.Candidates) != 1 {
		t.Fatalf("explanation = %+v", explanation)
	}
	if explanation.Candidates[0].Error != "route policy max output 256 exceeded by request 512" {
		t.Fatalf("candidate error = %q", explanation.Candidates[0].Error)
	}
	if explanation.Candidates[0].Route.MaxOutputTokens == nil || *explanation.Candidates[0].Route.MaxOutputTokens != 256 {
		t.Fatalf("candidate = %+v", explanation.Candidates[0])
	}
}

func TestPlannerExplainShowsRuntimeUsageBudgetRejection(t *testing.T) {
	t.Parallel()

	catalog := stubCatalog{
		refs: []modelsdev.ModelRef{
			modelRef("openai", "gpt-4o-mini", 128000, 16000, 0.15, 0.6, modelsdev.Capabilities{}, []string{"text"}, []string{"text"}),
		},
	}
	maxCost := 1.0
	planner := NewWithEvaluators(catalog, Policy{
		Version: "v11",
		Routes: []Route{{
			Provider: "openai",
			Model:    "gpt-4o-mini",
			UsageBudget: UsageBudgetPolicy{
				Level:      "provider",
				MaxCostUSD: &maxCost,
				Window:     "month",
				Scope:      "caller",
			},
		}},
	}, stubPolicyEvaluator{err: "usage budget 1.000000 USD/month exceeded: spent 1.200000 USD"})

	explanation := planner.Explain(llm.Request{
		Operation: llm.OperationChat,
		CallerID:  "agent-1",
	})
	if len(explanation.Candidates) != 1 {
		t.Fatalf("explanation = %+v", explanation)
	}
	if explanation.Candidates[0].Error != "usage budget 1.000000 USD/month exceeded: spent 1.200000 USD" {
		t.Fatalf("candidate error = %q", explanation.Candidates[0].Error)
	}
}

func modelRef(provider, id string, contextWindow, maxOutput int, inputCost, outputCost float64, caps modelsdev.Capabilities, inputMods, outputMods []string) modelsdev.ModelRef {
	return modelsdev.ModelRef{
		ProviderID: provider,
		ID:         id,
		Cost: modelsdev.Pricing{
			Input:  inputCost,
			Output: outputCost,
		},
		Limit: modelsdev.Limits{
			ContextWindow:   contextWindow,
			MaxOutputTokens: maxOutput,
		},
		Capabilities: caps,
		Modality: modelsdev.Modality{
			Input:  inputMods,
			Output: outputMods,
		},
	}
}

type stubCatalog struct {
	refs []modelsdev.ModelRef
}

type stubPolicyEvaluator struct {
	err string
}

func (s stubPolicyEvaluator) Validate(llm.Request, Route, float64) error {
	if s.err == "" {
		return nil
	}
	return errors.New(s.err)
}

func (s stubCatalog) List() []modelsdev.ModelRef {
	return append([]modelsdev.ModelRef(nil), s.refs...)
}

func (s stubCatalog) Get(providerID, modelID string) (modelsdev.Model, bool) {
	for _, ref := range s.refs {
		if ref.ProviderID == providerID && ref.ID == modelID {
			return modelsdev.Model{
				ID:           ref.ID,
				Cost:         ref.Cost,
				Limit:        ref.Limit,
				Modality:     ref.Modality,
				Capabilities: ref.Capabilities,
			}, true
		}
	}
	return modelsdev.Model{}, false
}

func (s stubCatalog) Capabilities(providerID, modelID string) (modelsdev.Capabilities, bool) {
	for _, ref := range s.refs {
		if ref.ProviderID == providerID && ref.ID == modelID {
			return ref.Capabilities, true
		}
	}
	return modelsdev.Capabilities{}, false
}

func (s stubCatalog) Modality(providerID, modelID string) (modelsdev.Modality, bool) {
	for _, ref := range s.refs {
		if ref.ProviderID == providerID && ref.ID == modelID {
			return ref.Modality, true
		}
	}
	return modelsdev.Modality{}, false
}

func (s stubCatalog) ContextWindow(providerID, modelID string) (int, bool) {
	for _, ref := range s.refs {
		if ref.ProviderID == providerID && ref.ID == modelID {
			return ref.Limit.ContextWindow, true
		}
	}
	return 0, false
}

func (s stubCatalog) MaxOutput(providerID, modelID string) (int, bool) {
	for _, ref := range s.refs {
		if ref.ProviderID == providerID && ref.ID == modelID {
			return ref.Limit.MaxOutputTokens, true
		}
	}
	return 0, false
}

func (s stubCatalog) EstimateCost(providerID, modelID string, promptTokens, completionTokens int) (float64, modelcatalog.PriceState) {
	for _, ref := range s.refs {
		if ref.ProviderID == providerID && ref.ID == modelID {
			cost := float64(promptTokens)*ref.Cost.Input/1_000_000 + float64(completionTokens)*ref.Cost.Output/1_000_000
			if cost == 0 {
				return 0, modelcatalog.PriceFree
			}
			return cost, modelcatalog.PricePriced
		}
	}
	return 0, modelcatalog.PriceUnknown
}

func TestValidateBudget(t *testing.T) {
	c := stubCatalog{refs: []modelsdev.ModelRef{
		{ProviderID: "p1", ID: "priced", Cost: modelsdev.Pricing{Input: 1.0, Output: 2.0}},
		{ProviderID: "p1", ID: "free", Cost: modelsdev.Pricing{Input: 0, Output: 0}},
	}}

	tests := []struct {
		name      string
		reqBudget float64
		routeMax  *float64
		model     string
		wantCost  float64
		wantOk    bool
		wantErr   bool
	}{
		{"no budget", 0, nil, "priced", 0, false, false},
		{"req budget sufficient", 5.0, nil, "priced", 0.002, true, false},
		{"req budget exceeded", 0.001, nil, "priced", 0, true, true},
		
		
		{"free model with zero budget passes", 0.0, func(f float64) *float64 { return &f }(0.0), "free", 0, true, false},
		{"unknown model with zero budget fails", 0.0, func(f float64) *float64 { return &f }(0.0), "unknown", 0, false, true},
		{"regression: max cost but no req budget passes priced", 0.0, func(f float64) *float64 { return &f }(5.0), "priced", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := llm.Request{
				CostBudgetUSD:   tt.reqBudget,
				MaxOutputTokens: 1000, // triggers token estimation
				Input:           []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "hello"}}}},
			}
			route := Route{MaxCostUSD: tt.routeMax}
			cost, ok, err := validateBudget(req, route, c, "p1", tt.model)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateBudget() error = %v, wantErr %v", err, tt.wantErr)
			}
			if ok != tt.wantOk {
				t.Errorf("validateBudget() ok = %v, want %v", ok, tt.wantOk)
			}
			if cost != tt.wantCost {
				t.Errorf("validateBudget() cost = %v, want %v", cost, tt.wantCost)
			}
		})
	}
}
