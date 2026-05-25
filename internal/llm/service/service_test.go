package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/observability"
	"github.com/hollis-labs/tether/internal/llm/router"
)

func TestServiceChatRoutesAndAppliesMiddleware(t *testing.T) {
	t.Parallel()

	var events []string
	svc := Service{
		Planner: stubPlanner{
			plan: router.Plan{
				Provider:         "anthropic-work",
				Model:            "claude-sonnet-4-5",
				EstimatedCostUSD: 0.0025,
				Reasons:          []string{"matched provider hint"},
				PolicyVersion:    "v1",
			},
		},
		Providers: map[string]llm.ChatProvider{
			"anthropic-work": stubChatProvider{
				resp: llm.Response{
					Output: []llm.Message{{
						Role:  "assistant",
						Parts: []llm.ContentPart{{Type: "text", Text: "hello"}},
					}},
					Usage: llm.Usage{InputTokens: 10, OutputTokens: 4},
				},
				events: &events,
			},
		},
		ProviderInfo: map[string]ProviderInfo{
			"anthropic-work": {ID: "anthropic-work", Type: "anthropic", DefaultModel: "claude-sonnet-4-5", Models: []string{"claude-sonnet-4-5"}},
		},
		RouteOrder: []string{"anthropic-work"},
		Middleware: []llm.Middleware{
			recordingMiddleware{name: "outer", events: &events},
			recordingMiddleware{name: "inner", events: &events},
		},
	}

	resp, err := svc.Chat(context.Background(), llm.Request{Operation: llm.OperationChat})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}

	wantEvents := []string{
		"enter:outer",
		"enter:inner",
		"provider",
		"exit:inner",
		"exit:outer",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if resp.Provider != "anthropic-work" || resp.Model != "claude-sonnet-4-5" {
		t.Fatalf("response provider/model = %q/%q", resp.Provider, resp.Model)
	}
	if resp.Usage.EstimatedCostUSD != 0.0025 {
		t.Fatalf("EstimatedCostUSD = %f", resp.Usage.EstimatedCostUSD)
	}
	if !reflect.DeepEqual(resp.Route, router.Plan{
		Provider:         "anthropic-work",
		Model:            "claude-sonnet-4-5",
		EstimatedCostUSD: 0.0025,
		Reasons:          []string{"matched provider hint"},
		PolicyVersion:    "v1",
	}.RouteDecision()) {
		t.Fatalf("Route = %+v", resp.Route)
	}
}

func TestServiceChatRejectsUnsupportedOperation(t *testing.T) {
	t.Parallel()

	svc := Service{}
	_, err := svc.Chat(context.Background(), llm.Request{Operation: llm.OperationEmbedding})
	if !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatalf("Chat error = %v, want ErrUnsupportedOperation", err)
	}
}

func TestServiceChatErrorsOnMissingProvider(t *testing.T) {
	t.Parallel()

	svc := Service{
		Planner: stubPlanner{plan: router.Plan{Provider: "missing", Model: "demo"}},
	}
	_, err := svc.Chat(context.Background(), llm.Request{Operation: llm.OperationChat})
	if !errors.Is(err, ErrProviderNotRegistered) {
		t.Fatalf("Chat error = %v, want ErrProviderNotRegistered", err)
	}
}

func TestServiceStreamChatRoutesAndEmitsFinalResponse(t *testing.T) {
	t.Parallel()

	var seen []llm.StreamEvent
	svc := Service{
		Planner: stubPlanner{
			plan: router.Plan{
				Provider:      "openai-work",
				Model:         "gpt-4o-mini",
				PolicyVersion: "v1",
			},
		},
		Providers: map[string]llm.ChatProvider{
			"openai-work": stubStreamingProvider{
				resp: llm.Response{
					Output: []llm.Message{{Role: "assistant", Parts: []llm.ContentPart{{Type: "text", Text: "hello"}}}},
					Usage:  llm.Usage{InputTokens: 2, OutputTokens: 1},
				},
				events: []llm.StreamEvent{
					{Kind: llm.StreamEventTextDelta, Delta: "hel"},
					{Kind: llm.StreamEventTextDelta, Delta: "lo"},
				},
			},
		},
	}

	resp, err := svc.StreamChat(context.Background(), llm.Request{Operation: llm.OperationChat}, func(ev llm.StreamEvent) error {
		seen = append(seen, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat err = %v", err)
	}
	if len(seen) != 4 || seen[0].Kind != llm.StreamEventStart || seen[1].Delta != "hel" || seen[2].Delta != "lo" || seen[3].Kind != llm.StreamEventCompleted {
		t.Fatalf("seen = %+v", seen)
	}
	if resp.Provider != "openai-work" || resp.Model != "gpt-4o-mini" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestServicePreviewRoute(t *testing.T) {
	t.Parallel()

	svc := Service{
		Planner: stubPlanner{plan: router.Plan{Provider: "anthropic-work", Model: "claude-sonnet-4-5"}},
	}
	plan, err := svc.PreviewRoute(llm.Request{Operation: llm.OperationChat})
	if err != nil {
		t.Fatalf("PreviewRoute err = %v", err)
	}
	if plan.Provider != "anthropic-work" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestServiceListProvidersAndModels(t *testing.T) {
	t.Parallel()

	svc := Service{
		Catalog: stubCatalog{refs: []modelsdev.ModelRef{
			{ProviderID: "anthropic", ID: "claude-sonnet-4-5"},
			{ProviderID: "anthropic", ID: "claude-opus-4-1"},
			{ProviderID: "openai", ID: "gpt-4o-mini"},
			{ProviderID: "openai", ID: "gpt-5"},
		}},
		ProviderInfo: map[string]ProviderInfo{
			"anthropic-work": {ID: "anthropic-work", Type: "anthropic", DefaultModel: "claude-sonnet-4-5", Models: []string{"claude-sonnet-4-5", "claude-opus-4-1"}},
			"openai-work":    {ID: "openai-work", Type: "openai", DefaultModel: "gpt-5", Models: []string{"gpt-5"}},
		},
		RouteOrder: []string{"anthropic-work", "openai-work"},
		Routes: []router.Route{
			{Provider: "anthropic-work", Model: "claude-sonnet-4-5"},
			{Provider: "openai-work", Model: "gpt-5", Mode: "summarize"},
		},
	}

	providers := svc.ListProviders()
	if len(providers) != 2 || providers[0].ID != "anthropic-work" || providers[1].ID != "openai-work" {
		t.Fatalf("providers = %+v", providers)
	}

	models := svc.ListModels("anthropic-work")
	if len(models) != 2 {
		t.Fatalf("models len = %d, want 2", len(models))
	}
	for _, m := range models {
		if m.ProviderID != "anthropic" {
			t.Fatalf("unexpected model provider = %q", m.ProviderID)
		}
	}

	openAIModels := svc.ListModels("openai-work")
	if len(openAIModels) != 1 || openAIModels[0].ID != "gpt-5" {
		t.Fatalf("openai models = %+v", openAIModels)
	}

	routes := svc.ListRoutes()
	if len(routes) != 2 || routes[1].Mode != "summarize" {
		t.Fatalf("routes = %+v", routes)
	}
}

func TestServicePreviewRouteRecordsBudgetRejectionAudit(t *testing.T) {
	t.Parallel()

	recorder := &stubRecorder{}
	publisher := &stubPublisher{}
	svc := Service{
		Planner: stubPlanner{
			err: errors.New("no route satisfied the request"),
			explanation: router.Explanation{
				PolicyVersion: "v1",
				Error:         "no route satisfied the request",
				Candidates: []router.CandidateExplanation{{
					Route: router.Route{Provider: "anthropic-work", Model: "claude-sonnet-4-5"},
					Error: "usage budget 1.000000 USD/month exceeded: spent 1.200000 USD",
				}},
			},
		},
		Recorder:  recorder,
		Publisher: publisher,
	}

	_, err := svc.PreviewRoute(llm.Request{Operation: llm.OperationChat, CallerID: "agent-1"})
	if err == nil {
		t.Fatal("PreviewRoute error = nil, want non-nil")
	}
	if len(recorder.events) != 2 {
		t.Fatalf("events len = %d, want 2", len(recorder.events))
	}
	if recorder.events[0].EventType != "budget_rejection" || recorder.events[0].Provider != "anthropic-work" {
		t.Fatalf("budget rejection event = %+v", recorder.events[0])
	}
	if recorder.events[1].EventType != "route_preview" {
		t.Fatalf("preview event = %+v", recorder.events[1])
	}
	if len(publisher.events) != 1 || publisher.events[0].Kind != events.KindAIBudgetRejected || publisher.events[0].Scope != events.ScopeDaemon {
		t.Fatalf("publisher events = %+v", publisher.events)
	}
	var payload struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Error    string `json:"error"`
		CallerID string `json:"caller_id"`
	}
	if err := json.Unmarshal([]byte(publisher.events[0].PayloadJSON), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Provider != "anthropic-work" || payload.Model != "claude-sonnet-4-5" || payload.CallerID != "agent-1" {
		t.Fatalf("payload = %+v", payload)
	}
}

type stubPlanner struct {
	plan        router.Plan
	err         error
	explanation router.Explanation
}

type stubCatalog struct {
	refs []modelsdev.ModelRef
}

func (s stubCatalog) List() []modelsdev.ModelRef {
	return append([]modelsdev.ModelRef(nil), s.refs...)
}

func (s stubPlanner) Plan(llm.Request) (router.Plan, error) {
	return s.plan, s.err
}

func (s stubPlanner) Explain(llm.Request) router.Explanation {
	if len(s.explanation.Candidates) > 0 || s.explanation.Winner != nil || s.explanation.Error != "" || s.explanation.PolicyVersion != "" {
		return s.explanation
	}
	if s.err != nil {
		return router.Explanation{Error: s.err.Error()}
	}
	plan := s.plan
	return router.Explanation{Winner: &plan, Candidates: []router.CandidateExplanation{{Route: router.Route{Provider: plan.Provider, Model: plan.Model}, Matched: true, Selected: true}}}
}

type stubChatProvider struct {
	resp   llm.Response
	err    error
	events *[]string
}

func (s stubChatProvider) Chat(_ context.Context, _ llm.Request, _ llm.RouteDecision) (llm.Response, error) {
	if s.events != nil {
		*s.events = append(*s.events, "provider")
	}
	return s.resp, s.err
}

type stubStreamingProvider struct {
	resp   llm.Response
	err    error
	events []llm.StreamEvent
}

func (s stubStreamingProvider) Chat(_ context.Context, _ llm.Request, _ llm.RouteDecision) (llm.Response, error) {
	return s.resp, s.err
}

func (s stubStreamingProvider) StreamChat(_ context.Context, _ llm.Request, _ llm.RouteDecision, emit func(llm.StreamEvent) error) (llm.Response, error) {
	for _, ev := range s.events {
		if err := emit(ev); err != nil {
			return llm.Response{}, err
		}
	}
	return s.resp, s.err
}

type recordingMiddleware struct {
	name   string
	events *[]string
}

func (m recordingMiddleware) Handle(ctx context.Context, req llm.Request, next llm.Handler) (llm.Response, error) {
	*m.events = append(*m.events, "enter:"+m.name)
	resp, err := next(ctx, req)
	*m.events = append(*m.events, "exit:"+m.name)
	return resp, err
}

type stubRecorder struct {
	events []observability.AuditEvent
}

func (s *stubRecorder) RecordAIAuditEvent(ev observability.AuditEvent) error {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	s.events = append(s.events, ev)
	return nil
}

type stubPublisher struct {
	events []events.Event
}

func (s *stubPublisher) Publish(_ context.Context, ev events.Event) error {
	s.events = append(s.events, ev)
	return nil
}
