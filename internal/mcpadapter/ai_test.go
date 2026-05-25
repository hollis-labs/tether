package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
	"github.com/hollis-labs/tether/internal/store"
)

func TestAITools_ListProvidersAndChat(t *testing.T) {
	a := newAIAdapter(t, []string{ScopeAIInvoke})

	res := callAITool(t, a, "mux_ai_list_providers", nil)
	if res.IsError {
		t.Fatalf("list providers error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if count, _ := body["count"].(float64); int(count) != 1 {
		t.Fatalf("providers body = %v", body)
	}

	res = callAITool(t, a, "mux_ai_list_routes", nil)
	if res.IsError {
		t.Fatalf("list routes error: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	if count, _ := body["count"].(float64); int(count) != 1 {
		t.Fatalf("routes body = %v", body)
	}

	res = callAITool(t, a, "mux_ai_route_explain", map[string]any{"text": "hello"})
	if res.IsError {
		t.Fatalf("route explain error: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	if count, _ := body["count"].(float64); int(count) != 1 {
		t.Fatalf("route explain body = %v", body)
	}

	res = callAITool(t, a, "mux_ai_chat", map[string]any{
		"text":     "hello",
		"provider": "anthropic-work",
	})
	if res.IsError {
		t.Fatalf("chat error: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	resp, _ := body["response"].(map[string]any)
	if got, _ := resp["provider"].(string); got != "anthropic-work" {
		t.Fatalf("response = %v", body)
	}
}

func TestAITools_ChatRequiresScope(t *testing.T) {
	a := newAIAdapter(t, nil)
	res := callAITool(t, a, "mux_ai_chat", map[string]any{"text": "hello"})
	if !res.IsError {
		t.Fatal("expected auth error")
	}
	body := parseToolJSON(t, res)
	if got := body["code"]; got != "insufficient_scope" {
		t.Fatalf("code = %v, want insufficient_scope", got)
	}
}

func TestAITools_ChatStreamSendsNotificationsAndReturnsFinalResponse(t *testing.T) {
	h := api.NewHandler(api.Deps{
		AI: aiStubService{
			stream: []llm.StreamEvent{
				{Kind: llm.StreamEventStart, Provider: "anthropic-work", Model: "claude-sonnet-4-5"},
				{Kind: llm.StreamEventTextDelta, Provider: "anthropic-work", Model: "claude-sonnet-4-5", Delta: "hello"},
			},
			streamResp: llm.Response{
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
	srv := httptest.NewServer(h)
	defer srv.Close()

	hostport := srv.URL[len("http://"):]
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+hostport), "test-token", []string{ScopeAIInvoke})
	s := mcpserver.NewMCPServer("test", version, mcpserver.WithToolCapabilities(true))
	a.registerAITools(s)

	req := mcp.CallToolRequest{}
	req.Params.Name = "mux_ai_chat_stream"
	req.Params.Arguments = map[string]any{"text": "hello"}
	req.Params.Meta = &mcp.Meta{ProgressToken: "tok-1"}
	session := &fakeLoggingSession{
		id:            "stdio",
		initialized:   true,
		notifications: make(chan mcp.JSONRPCNotification, 16),
		level:         mcp.LoggingLevelInfo,
	}
	ctx := s.WithContext(context.Background(), session)

	res, err := a.handleAIChatStream(ctx, req)
	if err != nil {
		t.Fatalf("handleAIChatStream: %v", err)
	}
	if res.IsError {
		t.Fatalf("stream tool error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if got, _ := body["streamed"].(bool); !got {
		t.Fatalf("stream body = %v", body)
	}
	if got, _ := body["event_count"].(float64); int(got) != 3 {
		t.Fatalf("event_count = %v body=%v", got, body)
	}

	var methods []string
	var payloads []string
	for {
		select {
		case notification := <-session.notifications:
			methods = append(methods, notification.Method)
			data, _ := json.Marshal(notification.Params)
			payloads = append(payloads, string(data))
		default:
			goto done
		}
	}
done:
	joinedMethods := strings.Join(methods, ",")
	joinedPayloads := strings.Join(payloads, "\n")
	if !strings.Contains(joinedMethods, "notifications/ai/chat_stream") {
		t.Fatalf("methods = %v payloads=%s", methods, joinedPayloads)
	}
	if !strings.Contains(joinedMethods, "notifications/message") {
		t.Fatalf("methods = %v payloads=%s", methods, joinedPayloads)
	}
	if !strings.Contains(joinedMethods, "notifications/progress") {
		t.Fatalf("methods = %v payloads=%s", methods, joinedPayloads)
	}
	if !strings.Contains(joinedPayloads, "response.output_text.delta") || !strings.Contains(joinedPayloads, "tok-1") {
		t.Fatalf("payloads = %s", joinedPayloads)
	}
}

type fakeLoggingSession struct {
	id            string
	initialized   bool
	notifications chan mcp.JSONRPCNotification
	level         mcp.LoggingLevel
}

func (s *fakeLoggingSession) Initialize()       { s.initialized = true }
func (s *fakeLoggingSession) Initialized() bool { return s.initialized }
func (s *fakeLoggingSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.notifications
}
func (s *fakeLoggingSession) SessionID() string                  { return s.id }
func (s *fakeLoggingSession) SetLogLevel(level mcp.LoggingLevel) { s.level = level }
func (s *fakeLoggingSession) GetLogLevel() mcp.LoggingLevel      { return s.level }

func TestAITools_UsageAndAudit(t *testing.T) {
	a := newAIAdapter(t, nil)

	res := callAITool(t, a, "mux_ai_usage", map[string]any{"provider": "anthropic-work"})
	if res.IsError {
		t.Fatalf("usage error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	summary, _ := body["summary"].(map[string]any)
	if got, _ := summary["requests"].(float64); int(got) != 2 {
		t.Fatalf("usage body = %v", body)
	}

	res = callAITool(t, a, "mux_ai_audit", map[string]any{"event_type": "chat"})
	if res.IsError {
		t.Fatalf("audit error: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 1 {
		t.Fatalf("audit body = %v", body)
	}

	res = callAITool(t, a, "mux_ai_budgets", map[string]any{"caller_id": "agent-1"})
	if res.IsError {
		t.Fatalf("budgets error: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 1 {
		t.Fatalf("budgets body = %v", body)
	}

	res = callAITool(t, a, "mux_ai_budget_alerts", map[string]any{"provider": "anthropic-work"})
	if res.IsError {
		t.Fatalf("budget alerts error: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 1 {
		t.Fatalf("budget alerts body = %v", body)
	}
	alerts, _ := body["alerts"].([]any)
	if len(alerts) != 1 {
		t.Fatalf("alerts = %v", body)
	}
}

func TestAITools_WaitBudgetAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/stream" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if got := q["scope"]; len(got) != 1 || got[0] != "daemon" {
			t.Fatalf("scope query = %v", got)
		}
		if got := q["kind"]; len(got) != 1 || got[0] != "ai.budget_rejected" {
			t.Fatalf("kind query = %v", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"id: 12\n" +
				"event: ai.budget_rejected\n" +
				`data: {"scope":"daemon","payload_json":"{\"provider\":\"anthropic-work\",\"model\":\"claude-sonnet-4-5\",\"caller_id\":\"agent-1\",\"error\":\"usage budget exceeded\"}"}` + "\n\n",
		))
	}))
	defer srv.Close()

	hostport := srv.URL[len("http://"):]
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+hostport), "test-token", nil)

	res := callAITool(t, a, "mux_ai_wait_budget_alerts", map[string]any{
		"caller_id":  "agent-1",
		"wait_ms":    100,
		"max_events": 1,
	})
	if res.IsError {
		t.Fatalf("wait budget alerts error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if got, _ := body["count"].(float64); int(got) != 1 {
		t.Fatalf("wait alerts body = %v", body)
	}
	if got, _ := body["next_since_seq"].(float64); int(got) != 12 {
		t.Fatalf("next_since_seq = %v body=%v", got, body)
	}
}

func callAITool(t *testing.T, a *Adapter, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s := mcpserver.NewMCPServer("test", version, mcpserver.WithToolCapabilities(true))
	a.registerAITools(s)

	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer c.Close()
	if _, err := c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func newAIAdapter(t *testing.T, scopes []string) *Adapter {
	t.Helper()

	h := api.NewHandler(api.Deps{
		AI: aiStubService{
			resp: llm.Response{
				Provider:   "anthropic-work",
				Model:      "claude-sonnet-4-5",
				StopReason: "end_turn",
				Output: []llm.Message{{
					Role:  "assistant",
					Parts: []llm.ContentPart{{Type: "text", Text: "hello from mcp"}},
				}},
				Usage: llm.Usage{InputTokens: 12, OutputTokens: 7, EstimatedCostUSD: 0.0031},
			},
			plan: router.Plan{
				Provider:         "anthropic-work",
				Model:            "claude-sonnet-4-5",
				EstimatedCostUSD: 0.0031,
				Reasons:          []string{"matched provider hint"},
				PolicyVersion:    "catalog-ai-v1",
			},
			explanation: router.Explanation{
				PolicyVersion: "catalog-ai-v1",
				Winner: &router.Plan{
					Provider:      "anthropic-work",
					Model:         "claude-sonnet-4-5",
					PolicyVersion: "catalog-ai-v1",
				},
				Candidates: []router.CandidateExplanation{
					{Route: router.Route{Provider: "anthropic-work", Model: "claude-sonnet-4-5", AllowTools: boolPtr(false), MaxOutputTokens: intPtr(512), MaxCostUSD: floatPtr(0.10), UsageBudget: router.UsageBudgetPolicy{Level: "route", MaxCostUSD: floatPtr(1.25), Window: "month", Scope: "caller"}}, Matched: true, Selected: true, Reasons: []string{"matched route"}},
				},
			},
			providers: []llmservice.ProviderInfo{
				{ID: "anthropic-work", Type: "anthropic", DefaultModel: "claude-sonnet-4-5", Models: []string{"claude-sonnet-4-5"}},
			},
			models: []modelsdev.ModelRef{{
				ProviderID: "anthropic",
				ID:         "claude-sonnet-4-5",
				Name:       "Claude Sonnet 4.5",
				Family:     "claude-sonnet-4",
				Limit:      modelsdev.Limits{ContextWindow: 200000, MaxOutputTokens: 16000},
				Modality:   modelsdev.Modality{Input: []string{"text"}, Output: []string{"text"}},
			}},
			routes: []router.Route{
				{Provider: "anthropic-work", Model: "claude-sonnet-4-5", RequiresReasoning: true, AllowTools: boolPtr(false), MaxOutputTokens: intPtr(512), MaxCostUSD: floatPtr(0.10), UsageBudget: router.UsageBudgetPolicy{Level: "route", MaxCostUSD: floatPtr(1.25), Window: "month", Scope: "caller"}},
			},
		},
		AIAudit: aiStubAuditStore{
			events: []store.AIEvent{{
				ID:        1,
				EventType: "budget_rejection",
				Operation: "chat",
				Provider:  "anthropic-work",
				Model:     "claude-sonnet-4-5",
				Success:   false,
				Error:     "usage budget 1.250000 USD/month exceeded: spent 1.300000 USD",
				Timestamp: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
			}},
		},
		AIUsage: aiStubUsageStore{
			summary: store.AIUsageSummary{
				Requests:         2,
				Successes:        2,
				Errors:           0,
				LatencyMs:        220,
				InputTokens:      50,
				OutputTokens:     18,
				EstimatedCostUSD: 0.0062,
			},
		},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	hostport := srv.URL[len("http://"):]
	return NewWithDaemon(&app.Service{}, client.New("tcp:"+hostport), "test-token", scopes)
}

type aiStubService struct {
	resp        llm.Response
	streamResp  llm.Response
	stream      []llm.StreamEvent
	plan        router.Plan
	explanation router.Explanation
	providers   []llmservice.ProviderInfo
	models      []modelsdev.ModelRef
	routes      []router.Route
}

func (s aiStubService) Chat(context.Context, llm.Request) (llm.Response, error) {
	return s.resp, nil
}

func (s aiStubService) StreamChat(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error) {
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
	return s.streamResp, nil
}

func (s aiStubService) PreviewRoute(llm.Request) (router.Plan, error) {
	return s.plan, nil
}

func (s aiStubService) ExplainRoute(llm.Request) (router.Explanation, error) {
	return s.explanation, nil
}

func (s aiStubService) ListProviders() []llmservice.ProviderInfo {
	return append([]llmservice.ProviderInfo(nil), s.providers...)
}

func (s aiStubService) ListModels(string) []modelsdev.ModelRef {
	return append([]modelsdev.ModelRef(nil), s.models...)
}

func (s aiStubService) ListRoutes() []router.Route {
	return append([]router.Route(nil), s.routes...)
}

type aiStubAuditStore struct {
	events []store.AIEvent
}

func (s aiStubAuditStore) QueryAIEvents(store.AIEventFilter) ([]store.AIEvent, error) {
	return append([]store.AIEvent(nil), s.events...), nil
}

type aiStubUsageStore struct {
	summary store.AIUsageSummary
}

func (s aiStubUsageStore) QueryAIUsageSummary(store.AIUsageFilter) (store.AIUsageSummary, error) {
	return s.summary, nil
}

func boolPtr(v bool) *bool {
	return &v
}

func intPtr(v int) *int {
	return &v
}

func floatPtr(v float64) *float64 {
	return &v
}

func TestAIRequestFromToolBuildsNormalizedRequest(t *testing.T) {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"text":              "hello",
		"system_prompt":     "be concise",
		"provider":          "anthropic-work",
		"cost_budget_usd":   0.5,
		"max_output_tokens": 64,
	}
	out, errRes := aiRequestFromTool(req)
	if errRes != nil {
		t.Fatalf("unexpected tool error: %s", textOf(errRes))
	}
	if out.ProviderHint != "anthropic-work" || out.CostBudgetUSD != 0.5 || len(out.Input) != 2 {
		b, _ := json.Marshal(out)
		t.Fatalf("request = %s", string(b))
	}
}

func TestAIRequestFromToolAcceptsRequestObject(t *testing.T) {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"request": map[string]any{
			"operation": "chat",
			"input": []any{
				map[string]any{
					"role": "user",
					"parts": []any{
						map[string]any{"type": "text", "text": "hello"},
					},
				},
			},
		},
		"provider":        "anthropic-work",
		"system_prompt":   "be concise",
		"request_id":      "req-123",
		"token_budget":    128,
		"cost_budget_usd": 0.2,
	}

	out, errRes := aiRequestFromTool(req)
	if errRes != nil {
		t.Fatalf("unexpected tool error: %s", textOf(errRes))
	}
	if out.ProviderHint != "anthropic-work" || out.RequestID != "req-123" || out.TokenBudget != 128 || len(out.Input) != 2 {
		b, _ := json.Marshal(out)
		t.Fatalf("request = %s", string(b))
	}
	if out.Input[0].Role != "system" || out.Input[1].Role != "user" {
		b, _ := json.Marshal(out)
		t.Fatalf("request roles = %s", string(b))
	}
}
