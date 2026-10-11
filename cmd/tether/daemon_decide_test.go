package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/llm"
	llmjev "github.com/hollis-labs/tether/internal/llm/jev"
	"github.com/hollis-labs/tether/internal/llm/router"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
	"github.com/hollis-labs/tether/internal/modelcatalog"
)

func TestSplitDecisionRoutes(t *testing.T) {
	routes := []router.Route{{Provider: "anthropic-work", Model: "a"}, {Provider: "jev", Model: "jev-1.13.0"}, {Provider: "openai", Model: "b"}}
	chat, decide := splitDecisionRoutes(routes, map[string]llm.DecisionProvider{"jev": nil})
	if len(chat) != 2 || len(decide) != 1 || decide[0].Provider != "jev" {
		t.Fatalf("chat=%+v decide=%+v", chat, decide)
	}
}

// A real planner and service over the jev adapter and a fake server: decide
// routes through the synthetic catalog model, chat never sees the provider.
func TestDecideEndToEndThroughPlannerAndAdapter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.7}},"usage":{"input_tokens":2000000,"output_tokens":3}}`))
	}))
	defer srv.Close()

	price := 0.042
	pc := config.AIProviderConfig{ID: "jev", Type: "jev", Models: []string{"jev-1.13.0"}, SecretRef: "keychain://x", Enabled: true, InputPriceUSDPerMTok: &price, BaseURL: srv.URL}
	cfg := jevAdapterConfig(pc, func(context.Context) (string, error) { return "key", nil })
	provider := llmjev.New(cfg)

	models, prices := syntheticConfiguredModels(map[string]config.AIProviderConfig{"jev": pc})
	catalog := modelcatalog.NewOverlay(modelcatalog.New(), models, prices)
	routes := []router.Route{{Provider: "jev", CatalogProvider: effectiveCatalogProvider(pc), Model: "jev-1.13.0"}}

	svc := &llmservice.Service{
		DecisionPlanner:   router.New(catalog, router.Policy{Version: "decide-v1", Routes: routes}),
		DecisionProviders: map[string]llm.DecisionProvider{"jev": provider},
		ProviderInfo:      map[string]llmservice.ProviderInfo{"jev": {ID: "jev", Type: "jev"}},
		DecisionRoutes:    routes,
	}

	resp, err := svc.Decide(context.Background(), llm.Request{
		Operation: llm.OperationDecide,
		Decision: &llm.DecisionRequest{
			State:     "text",
			Purpose:   "p",
			Questions: []llm.DecisionQuestion{{Key: "urgent", Type: llm.QuestionNoul, Instructions: "urgent?"}},
		},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if resp.Provider != "jev" || resp.Route.PolicyVersion != "decide-v1" || resp.Decision.Answers["urgent"].Noul != 0.7 {
		t.Fatalf("resp = %+v", resp)
	}
	if got := resp.Usage.EstimatedCostUSD; got < 0.0839 || got > 0.0841 {
		t.Fatalf("cost = %v, want ~0.084 (2M input tokens at $0.042/M)", got)
	}

	// Chat has no planner here: it must fail, not wander onto the decision provider.
	if _, err := svc.Chat(context.Background(), llm.Request{Operation: llm.OperationChat}); err == nil {
		t.Fatal("chat unexpectedly routed")
	}

	// Without a provider hint matching, a missing key surfaces as a typed auth error.
	bad := llmjev.New(llmjev.Config{BaseURL: srv.URL, ResolveAPIKey: func(context.Context) (string, error) { return "", errors.New("nope") }})
	svc.DecisionProviders["jev"] = bad
	_, err = svc.Decide(context.Background(), llm.Request{
		Operation: llm.OperationDecide,
		Decision: &llm.DecisionRequest{
			State: "t", OnError: llm.OnErrorFailClosed,
			Questions: []llm.DecisionQuestion{{Key: "q", Type: llm.QuestionNoul, Instructions: "i"}},
		},
	})
	var de *llm.DecisionError
	if !errors.As(err, &de) || de.Kind != llm.DecisionErrAuth || de.OnError != llm.OnErrorFailClosed || strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}
