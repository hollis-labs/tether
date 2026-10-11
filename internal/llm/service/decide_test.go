package service

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
)

type stubDecisionProvider struct {
	resp  llm.Response
	err   error
	calls int
}

func (s *stubDecisionProvider) Decide(_ context.Context, _ llm.Request, _ llm.RouteDecision) (llm.Response, error) {
	s.calls++
	return s.resp, s.err
}

func decideRequest() llm.Request {
	return llm.Request{
		Operation: llm.OperationDecide,
		RequestID: "r1",
		Decision: &llm.DecisionRequest{
			State:   "task text",
			Purpose: "task-kind",
			OnError: llm.OnErrorFailOpen,
			Questions: []llm.DecisionQuestion{
				{Key: "urgent", Type: llm.QuestionNoul, Instructions: "Is it urgent?"},
			},
		},
	}
}

func decideService(p *stubDecisionProvider, rec *stubRecorder) Service {
	svc := Service{
		DecisionPlanner: stubPlanner{plan: router.Plan{Provider: "jev", Model: "jev-1.13.0", PolicyVersion: "d1"}},
		DecisionProviders: map[string]llm.DecisionProvider{
			"jev": p,
		},
		ProviderInfo: map[string]ProviderInfo{"jev": {ID: "jev", Type: "jev", DefaultModel: "jev-1.13.0", Models: []string{"jev-1.13.0"}}},
	}
	if rec != nil {
		svc.Recorder = rec
	}
	return svc
}

func TestServiceDecideRoutesAndAudits(t *testing.T) {
	t.Parallel()

	p := &stubDecisionProvider{resp: llm.Response{
		Usage:    llm.Usage{InputTokens: 1000, OutputTokens: 5, EstimatedCostUSD: 0.000042},
		Decision: &llm.DecisionResult{Answers: map[string]llm.DecisionAnswer{"urgent": {Type: llm.QuestionNoul, Noul: 0.9}}},
	}}
	rec := &stubRecorder{}
	svc := decideService(p, rec)

	resp, err := svc.Decide(context.Background(), decideRequest())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if resp.Provider != "jev" || resp.Model != "jev-1.13.0" || resp.Route.PolicyVersion != "d1" {
		t.Fatalf("envelope: %+v", resp)
	}
	if resp.Decision.RequestID != "r1" {
		t.Fatalf("request id not stamped: %+v", resp.Decision)
	}
	if len(rec.events) != 1 {
		t.Fatalf("events: %+v", rec.events)
	}
	ev := rec.events[0]
	if ev.EventType != "decide" || ev.Operation != "decide" || ev.Purpose != "task-kind" || ev.EstimatedCostUSD != 0.000042 || ev.InputTokens != 1000 {
		t.Fatalf("audit event: %+v", ev)
	}
	// Question text and state never reach the audit trail.
	if got := ev.RequestSummary; got == "" || contains(got, "task text") || contains(got, "urgent") {
		t.Fatalf("request summary leaks content or is empty: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestServiceDecideProviderErrorKeepsTypeAndIsAudited(t *testing.T) {
	t.Parallel()

	p := &stubDecisionProvider{err: &llm.DecisionError{Kind: llm.DecisionErrRateLimited, Status: 429, OnError: llm.OnErrorFailOpen}}
	rec := &stubRecorder{}
	svc := decideService(p, rec)

	_, err := svc.Decide(context.Background(), decideRequest())
	var de *llm.DecisionError
	if !errors.As(err, &de) || de.Kind != llm.DecisionErrRateLimited || de.OnError != llm.OnErrorFailOpen {
		t.Fatalf("err = %v", err)
	}
	if len(rec.events) == 0 || rec.events[len(rec.events)-1].Success {
		t.Fatalf("failure must be audited: %+v", rec.events)
	}
}

func TestServiceDecideWithoutDecisionRoutes(t *testing.T) {
	t.Parallel()

	svc := Service{Planner: stubPlanner{}}
	_, err := svc.Decide(context.Background(), decideRequest())
	if !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceDecideRejectsInvalidRequestBeforeRouting(t *testing.T) {
	t.Parallel()

	p := &stubDecisionProvider{}
	svc := decideService(p, nil)
	req := decideRequest()
	req.Decision.Questions = nil
	if _, err := svc.Decide(context.Background(), req); !errors.Is(err, llm.ErrInvalidDecision) {
		t.Fatalf("err = %v", err)
	}
	if p.calls != 0 {
		t.Fatal("invalid request reached the provider")
	}
}

func TestServiceChatNeverReachesDecisionRoutes(t *testing.T) {
	t.Parallel()

	p := &stubDecisionProvider{}
	svc := decideService(p, nil)
	// No chat planner: a chat request must not fall through to a decision provider.
	if _, err := svc.Chat(context.Background(), llm.Request{Operation: llm.OperationChat}); err == nil {
		t.Fatal("chat unexpectedly succeeded")
	}
	if p.calls != 0 {
		t.Fatal("chat reached a decision provider")
	}
}

func TestServicePreviewAndExplainUseDecisionPlanner(t *testing.T) {
	t.Parallel()

	svc := decideService(&stubDecisionProvider{}, nil)
	svc.Planner = stubPlanner{plan: router.Plan{Provider: "chat-provider", Model: "m"}}

	plan, err := svc.PreviewRoute(decideRequest())
	if err != nil || plan.Provider != "jev" {
		t.Fatalf("preview: %+v %v", plan, err)
	}
	ex, err := svc.ExplainRoute(decideRequest())
	if err != nil || ex.Winner == nil || ex.Winner.Provider != "jev" {
		t.Fatalf("explain: %+v %v", ex, err)
	}
}

func TestServiceListRoutesIncludesDecisionRoutes(t *testing.T) {
	t.Parallel()

	svc := Service{
		Routes:         []router.Route{{Provider: "a", Model: "m"}},
		DecisionRoutes: []router.Route{{Provider: "jev", Model: "jev-1.13.0"}},
	}
	if got := svc.ListRoutes(); len(got) != 2 || got[1].Provider != "jev" {
		t.Fatalf("routes: %+v", got)
	}
}
