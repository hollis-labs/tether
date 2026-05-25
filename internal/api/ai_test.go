package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
	"github.com/hollis-labs/tether/internal/store"
)

func TestAIChatReturnsResponse(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI: stubAIService{
			resp: llm.Response{
				Provider:   "anthropic-work",
				Model:      "claude-sonnet-4-5",
				StopReason: "end_turn",
				Output: []llm.Message{{
					Role:  "assistant",
					Parts: []llm.ContentPart{{Type: "text", Text: "hello"}},
				}},
			},
		},
	})

	body := bytes.NewBufferString(`{"request":{"input":[{"role":"user","parts":[{"type":"text","text":"hi"}]}]}}`)
	req := httptest.NewRequest(http.MethodPost, "/ai/chat", body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var resp ChatResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Response.Provider != "anthropic-work" || resp.Response.Model != "claude-sonnet-4-5" {
		t.Fatalf("response = %+v", resp.Response)
	}
}

func TestAIChatRejectsNonChatOperation(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{AI: stubAIService{}})
	req := httptest.NewRequest(http.MethodPost, "/ai/chat", bytes.NewBufferString(`{"request":{"operation":"embedding"}}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestAIChatStreamReturnsSSE(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI: stubAIService{
			stream: []llm.StreamEvent{
				{Kind: llm.StreamEventTextDelta, Provider: "anthropic-work", Model: "claude-sonnet-4-5", Delta: "hello"},
			},
			streamResp: llm.Response{
				Provider:   "anthropic-work",
				Model:      "claude-sonnet-4-5",
				StopReason: "end_turn",
				Output:     []llm.Message{{Role: "assistant", Parts: []llm.ContentPart{{Type: "text", Text: "hello"}}}},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/ai/chat/stream", bytes.NewBufferString(`{"request":{"input":[{"role":"user","parts":[{"type":"text","text":"hi"}]}]}}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	body, err := io.ReadAll(rr.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	chunk := string(body)
	if !strings.Contains(chunk, "event: response.output_text.delta") || !strings.Contains(chunk, "\"delta\":\"hello\"") || !strings.Contains(chunk, "event: response.completed") {
		t.Fatalf("chunk = %q", chunk)
	}
}

func TestAIChatRouteAbsentWhenNotConfigured(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{})
	req := httptest.NewRequest(http.MethodPost, "/ai/chat", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestAIProvidersAndModels(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI: stubAIService{
			providers: []llmservice.ProviderInfo{
				{ID: "anthropic-work", Type: "anthropic", DefaultModel: "claude-sonnet-4-5", Models: []string{"claude-sonnet-4-5", "claude-opus-4-1"}},
				{ID: "openai-personal", Type: "openai", DefaultModel: "gpt-4o-mini", Models: []string{"gpt-4o-mini"}},
				{ID: "llama-local", Type: "openai-compatible", DefaultModel: "llama3.1", Models: []string{"llama3.1"}},
			},
			models: []modelsdev.ModelRef{
				{
					ProviderID: "anthropic",
					ID:         "claude-sonnet-4-5",
					Name:       "Claude Sonnet 4.5",
					Limit:      modelsdev.Limits{ContextWindow: 200000, MaxOutputTokens: 16000},
					Modality:   modelsdev.Modality{Input: []string{"text", "image"}, Output: []string{"text"}},
				},
				{
					ProviderID: "anthropic",
					ID:         "claude-opus-4-1",
					Name:       "Claude Opus 4.1",
					Limit:      modelsdev.Limits{ContextWindow: 200000, MaxOutputTokens: 32000},
					Modality:   modelsdev.Modality{Input: []string{"text", "image"}, Output: []string{"text"}},
				},
				{
					ProviderID: "openai",
					ID:         "gpt-4o-mini",
					Name:       "GPT-4o mini",
					Limit:      modelsdev.Limits{ContextWindow: 128000, MaxOutputTokens: 16000},
					Modality:   modelsdev.Modality{Input: []string{"text", "image"}, Output: []string{"text"}},
				},
				{
					ProviderID: "openai",
					ID:         "llama3.1",
					Name:       "Llama 3.1",
					Limit:      modelsdev.Limits{ContextWindow: 128000, MaxOutputTokens: 8192},
					Modality:   modelsdev.Modality{Input: []string{"text"}, Output: []string{"text"}},
				},
			},
			routes: []router.Route{
				{Provider: "anthropic-work", Model: "claude-sonnet-4-5", RequiresReasoning: true, AllowTools: boolRef(false), MaxOutputTokens: intRef(512), MaxCostUSD: floatRef(0.10), UsageBudget: router.UsageBudgetPolicy{Level: "route", MaxCostUSD: floatRef(1.25), Window: "month", Scope: "caller"}},
				{Provider: "openai-personal", Model: "gpt-4o-mini", Mode: "summarize"},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/ai/providers", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("providers status = %d", rr.Code)
	}
	var providersResp ListAIProvidersResponse
	if err := json.NewDecoder(rr.Body).Decode(&providersResp); err != nil {
		t.Fatalf("decode providers: %v", err)
	}
	if len(providersResp.Providers) != 3 || providersResp.Providers[0].ID != "anthropic-work" || providersResp.Providers[1].ID != "openai-personal" || providersResp.Providers[2].ID != "llama-local" {
		t.Fatalf("providers = %+v", providersResp.Providers)
	}
	if len(providersResp.Providers[0].Models) != 2 {
		t.Fatalf("provider models = %+v", providersResp.Providers[0])
	}

	req = httptest.NewRequest(http.MethodGet, "/ai/models?provider_id=anthropic-work", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("models status = %d", rr.Code)
	}
	var modelsResp ListAIModelsResponse
	if err := json.NewDecoder(rr.Body).Decode(&modelsResp); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(modelsResp.Models) != 2 || modelsResp.Models[0].ConfiguredProviderID != "anthropic-work" || modelsResp.Models[1].ConfiguredProviderID != "anthropic-work" {
		t.Fatalf("models = %+v", modelsResp.Models)
	}

	req = httptest.NewRequest(http.MethodGet, "/ai/models?provider_id=openai-personal", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("openai models status = %d", rr.Code)
	}
	if err := json.NewDecoder(rr.Body).Decode(&modelsResp); err != nil {
		t.Fatalf("decode openai models: %v", err)
	}
	if len(modelsResp.Models) != 1 || modelsResp.Models[0].ConfiguredProviderID != "openai-personal" {
		t.Fatalf("openai models = %+v", modelsResp.Models)
	}

	req = httptest.NewRequest(http.MethodGet, "/ai/models?provider_id=llama-local", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("compat models status = %d", rr.Code)
	}
	if err := json.NewDecoder(rr.Body).Decode(&modelsResp); err != nil {
		t.Fatalf("decode compat models: %v", err)
	}
	if len(modelsResp.Models) != 1 || modelsResp.Models[0].ConfiguredProviderID != "llama-local" {
		t.Fatalf("compat models = %+v", modelsResp.Models)
	}

	req = httptest.NewRequest(http.MethodGet, "/ai/routes", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("routes status = %d", rr.Code)
	}
	var routesResp ListAIRoutesResponse
	if err := json.NewDecoder(rr.Body).Decode(&routesResp); err != nil {
		t.Fatalf("decode routes: %v", err)
	}
	if len(routesResp.Routes) != 2 || !routesResp.Routes[0].RequiresReasoning || routesResp.Routes[0].AllowTools == nil || *routesResp.Routes[0].AllowTools || routesResp.Routes[0].MaxOutputTokens == nil || *routesResp.Routes[0].MaxOutputTokens != 512 || routesResp.Routes[0].UsageBudget == nil || routesResp.Routes[0].UsageBudget.Level != "route" || routesResp.Routes[1].Mode != "summarize" {
		t.Fatalf("routes = %+v", routesResp.Routes)
	}
}

func TestAIRoutePreview(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI: stubAIService{
			plan: router.Plan{
				Provider:         "anthropic-work",
				Model:            "claude-sonnet-4-5",
				EstimatedCostUSD: 0.003,
				Reasons:          []string{"matched provider hint"},
				PolicyVersion:    "catalog-ai-v1",
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/ai/routes/preview", bytes.NewBufferString(`{"request":{"input":[{"role":"user","parts":[{"type":"text","text":"hi"}]}]}}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp RoutePreviewResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode route preview: %v", err)
	}
	if !reflect.DeepEqual(resp.Route.Reasons, []string{"matched provider hint"}) {
		t.Fatalf("route = %+v", resp.Route)
	}
}

func TestAIRouteExplain(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI: stubAIService{
			explanation: router.Explanation{
				PolicyVersion: "catalog-ai-v1",
				Winner: &router.Plan{
					Provider:      "anthropic-work",
					Model:         "claude-sonnet-4-5",
					PolicyVersion: "catalog-ai-v1",
				},
				Candidates: []router.CandidateExplanation{
					{Route: router.Route{Provider: "anthropic-work", Model: "claude-sonnet-4-5", AllowTools: boolRef(false), MaxOutputTokens: intRef(512), MaxCostUSD: floatRef(0.10), UsageBudget: router.UsageBudgetPolicy{Level: "provider", MaxCostUSD: floatRef(2.00), Window: "day", Scope: "session"}}, Matched: true, Selected: true, Reasons: []string{"matched route"}},
					{Route: router.Route{Provider: "openai-work", Model: "gpt-5", Mode: "summarize"}, Reasons: []string{`route mode "summarize" did not match request mode ""`}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/ai/routes/explain", bytes.NewBufferString(`{"request":{"input":[{"role":"user","parts":[{"type":"text","text":"hi"}]}]}}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp RouteExplainResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode route explain: %v", err)
	}
	if resp.Winner == nil || len(resp.Candidates) != 2 || !resp.Candidates[0].Selected || resp.Candidates[0].AllowTools == nil || *resp.Candidates[0].AllowTools || resp.Candidates[0].MaxOutputTokens == nil || *resp.Candidates[0].MaxOutputTokens != 512 || resp.Candidates[0].UsageBudget == nil || resp.Candidates[0].UsageBudget.Scope != "session" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestAIAuditList(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI:      stubAIService{},
		AIAudit: stubAIAuditStore{events: []store.AIEvent{{ID: 1, EventType: "chat", Operation: "chat", Provider: "anthropic-work", Success: true, Timestamp: time.Now().UTC()}}},
	})
	req := httptest.NewRequest(http.MethodGet, "/ai/audit", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp AIAuditListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if len(resp.Events) != 1 || resp.Events[0].EventType != "chat" {
		t.Fatalf("events = %+v", resp.Events)
	}
}

func TestAIUsageSummary(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI: stubAIService{},
		AIUsage: &stubAIUsageStore{
			summary: store.AIUsageSummary{
				Requests:         2,
				Successes:        1,
				Errors:           1,
				LatencyMs:        300,
				InputTokens:      120,
				OutputTokens:     45,
				EstimatedCostUSD: 0.0125,
				ByProvider: []store.AIUsageBreakdown{{
					Key:              "anthropic-work",
					Requests:         2,
					Successes:        1,
					Errors:           1,
					LatencyMs:        300,
					InputTokens:      120,
					OutputTokens:     45,
					EstimatedCostUSD: 0.0125,
				}},
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/ai/usage?provider=anthropic-work", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp AIUsageResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if resp.Requests != 2 || len(resp.ByProvider) != 1 || resp.ByProvider[0].Key != "anthropic-work" {
		t.Fatalf("usage = %+v", resp)
	}
}

func TestAIBudgets(t *testing.T) {
	t.Parallel()

	usage := &stubAIUsageStore{
		summary: store.AIUsageSummary{EstimatedCostUSD: 0.60},
	}
	h := NewHandler(Deps{
		AI: stubAIService{
			routes: []router.Route{
				{Provider: "anthropic-work", Model: "claude-sonnet-4-5", UsageBudget: router.UsageBudgetPolicy{Level: "provider", MaxCostUSD: floatRef(1.00), Window: "month", Scope: "caller"}},
				{Provider: "openai-work", Model: "gpt-5", UsageBudget: router.UsageBudgetPolicy{Level: "global", MaxCostUSD: floatRef(5.00), Window: "day"}},
			},
		},
		AIUsage: usage,
	})
	req := httptest.NewRequest(http.MethodGet, "/ai/budgets?caller_id=agent-1&provider=anthropic-work", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp AIUsageBudgetsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode budgets: %v", err)
	}
	if resp.Count != 1 || len(resp.Budgets) != 1 {
		t.Fatalf("budgets = %+v", resp)
	}
	if resp.Budgets[0].RemainingCostUSD != 0.40 || resp.Budgets[0].Filter.CallerID != "agent-1" || resp.Budgets[0].UsageBudget.Level != "provider" {
		t.Fatalf("budget entry = %+v", resp.Budgets[0])
	}
	if usage.lastFilter.Provider != "anthropic-work" || usage.lastFilter.CallerID != "agent-1" {
		t.Fatalf("usage filter = %+v", usage.lastFilter)
	}
}

func TestAIBudgetsMarksMissingScope(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{
		AI: stubAIService{
			routes: []router.Route{
				{Provider: "anthropic-work", Model: "claude-sonnet-4-5", UsageBudget: router.UsageBudgetPolicy{Level: "provider", MaxCostUSD: floatRef(1.00), Window: "month", Scope: "caller"}},
			},
		},
		AIUsage: &stubAIUsageStore{},
	})
	req := httptest.NewRequest(http.MethodGet, "/ai/budgets", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp AIUsageBudgetsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode budgets: %v", err)
	}
	if len(resp.Budgets) != 1 || resp.Budgets[0].Error != "usage budget scope caller requires caller_id" {
		t.Fatalf("budgets = %+v", resp)
	}
}

type stubAIService struct {
	resp        llm.Response
	err         error
	streamResp  llm.Response
	streamErr   error
	stream      []llm.StreamEvent
	plan        router.Plan
	planErr     error
	explanation router.Explanation
	providers   []llmservice.ProviderInfo
	models      []modelsdev.ModelRef
	routes      []router.Route
}

func (s stubAIService) Chat(context.Context, llm.Request) (llm.Response, error) {
	return s.resp, s.err
}

func (s stubAIService) StreamChat(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error) {
	for _, ev := range s.stream {
		if err := emit(ev); err != nil {
			return llm.Response{}, err
		}
	}
	if s.streamResp.Provider != "" || s.streamResp.Model != "" || len(s.streamResp.Output) > 0 || s.streamResp.StopReason != "" {
		if err := emit(llm.StreamEvent{
			Kind:       llm.StreamEventCompleted,
			Provider:   s.streamResp.Provider,
			Model:      s.streamResp.Model,
			StopReason: s.streamResp.StopReason,
			Response:   &s.streamResp,
		}); err != nil {
			return llm.Response{}, err
		}
	}
	return s.streamResp, s.streamErr
}

func (s stubAIService) PreviewRoute(llm.Request) (router.Plan, error) {
	return s.plan, s.planErr
}

func (s stubAIService) ExplainRoute(llm.Request) (router.Explanation, error) {
	return s.explanation, nil
}

func (s stubAIService) ListProviders() []llmservice.ProviderInfo {
	return append([]llmservice.ProviderInfo(nil), s.providers...)
}

func (s stubAIService) ListModels(string) []modelsdev.ModelRef {
	return append([]modelsdev.ModelRef(nil), s.models...)
}

func (s stubAIService) ListRoutes() []router.Route {
	return append([]router.Route(nil), s.routes...)
}

type stubAIAuditStore struct {
	events []store.AIEvent
	err    error
}

func (s stubAIAuditStore) QueryAIEvents(store.AIEventFilter) ([]store.AIEvent, error) {
	return append([]store.AIEvent(nil), s.events...), s.err
}

type stubAIUsageStore struct {
	summary    store.AIUsageSummary
	err        error
	lastFilter store.AIUsageFilter
}

func (s *stubAIUsageStore) QueryAIUsageSummary(f store.AIUsageFilter) (store.AIUsageSummary, error) {
	s.lastFilter = f
	return s.summary, s.err
}

func boolRef(v bool) *bool {
	return &v
}

func intRef(v int) *int {
	return &v
}

func floatRef(v float64) *float64 {
	return &v
}
