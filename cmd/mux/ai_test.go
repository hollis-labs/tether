package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
	"github.com/hollis-labs/tether/internal/store"
)

type aiFixture struct {
	t   *testing.T
	srv *httptest.Server
}

func newAIFixture(t *testing.T) *aiFixture {
	t.Helper()
	t.Cleanup(resetAIFlags)

	h := api.NewHandler(api.Deps{
		AI: aiStubService{
			resp: llm.Response{
				Provider:   "anthropic-work",
				Model:      "claude-sonnet-4-5",
				StopReason: "end_turn",
				Output: []llm.Message{{
					Role:  "assistant",
					Parts: []llm.ContentPart{{Type: "text", Text: "hello from tether"}},
				}},
				Usage: llm.Usage{InputTokens: 12, OutputTokens: 7, EstimatedCostUSD: 0.0031},
			},
			streamResp: llm.Response{
				Provider:   "anthropic-work",
				Model:      "claude-sonnet-4-5",
				StopReason: "end_turn",
				Output: []llm.Message{{
					Role:  "assistant",
					Parts: []llm.ContentPart{{Type: "text", Text: "hello from tether"}},
				}},
				Usage: llm.Usage{InputTokens: 12, OutputTokens: 7, EstimatedCostUSD: 0.0031},
			},
			stream: []llm.StreamEvent{
				{Kind: llm.StreamEventTextDelta, Delta: "hello "},
				{Kind: llm.StreamEventTextDelta, Delta: "from tether"},
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
				{ID: "anthropic-work", Type: "anthropic", DefaultModel: "claude-sonnet-4-5"},
			},
			models: []modelsdev.ModelRef{{
				ProviderID: "anthropic",
				ID:         "claude-sonnet-4-5",
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
				ByProvider: []store.AIUsageBreakdown{{
					Key:              "anthropic-work",
					Requests:         2,
					Successes:        2,
					LatencyMs:        220,
					InputTokens:      50,
					OutputTokens:     18,
					EstimatedCostUSD: 0.0062,
				}},
			},
		},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	prevFactory := aiClientFactory
	aiClientFactory = func() (*client.Client, error) {
		hostport := strings.TrimPrefix(srv.URL, "http://")
		return client.New("tcp:" + hostport), nil
	}
	t.Cleanup(func() { aiClientFactory = prevFactory })

	return &aiFixture{t: t, srv: srv}
}

func TestAICmdProvidersJSON(t *testing.T) {
	_ = newAIFixture(t)
	aiJSONFlag = true

	out := captureStdout(t, func() {
		if err := aiProvidersCmd.RunE(aiProvidersCmd, nil); err != nil {
			t.Fatalf("providers: %v", err)
		}
	})

	var resp api.ListAIProvidersResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode providers json: %v\n%s", err, out)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].ID != "anthropic-work" {
		t.Fatalf("providers = %+v", resp.Providers)
	}
}

func TestAICmdChatPretty(t *testing.T) {
	_ = newAIFixture(t)

	stdout, stderr := captureStdStreams(t, func() {
		if err := aiChatCmd.RunE(aiChatCmd, []string{"hello"}); err != nil {
			t.Fatalf("chat: %v", err)
		}
	})
	if !strings.Contains(stdout, "hello from tether") {
		t.Fatalf("stdout missing assistant text: %q", stdout)
	}
	if !strings.Contains(stderr, "provider=anthropic-work") {
		t.Fatalf("stderr missing response metadata: %q", stderr)
	}
}

func TestAICmdChatStreamPretty(t *testing.T) {
	_ = newAIFixture(t)
	aiStreamFlag = true

	stdout, stderr := captureStdStreams(t, func() {
		if err := aiChatCmd.RunE(aiChatCmd, []string{"hello"}); err != nil {
			t.Fatalf("chat --stream: %v", err)
		}
	})
	if !strings.Contains(stdout, "hello from tether") {
		t.Fatalf("stdout missing streamed assistant text: %q", stdout)
	}
	if !strings.Contains(stderr, "provider=anthropic-work") {
		t.Fatalf("stderr missing stream response metadata: %q", stderr)
	}
}

func TestAICmdRoutesPretty(t *testing.T) {
	_ = newAIFixture(t)

	out := captureStdout(t, func() {
		if err := aiRoutesCmd.RunE(aiRoutesCmd, nil); err != nil {
			t.Fatalf("routes: %v", err)
		}
	})
	for _, want := range []string{"anthropic-work", "claude-sonnet-4-5", "true", "false", "512", "0.100000", "caller/month/1.250000@route"} {
		if !strings.Contains(out, want) {
			t.Fatalf("routes output missing %q: %s", want, out)
		}
	}
}

func TestAICmdRouteExplainPretty(t *testing.T) {
	_ = newAIFixture(t)

	out := captureStdout(t, func() {
		if err := aiRouteExplainCmd.RunE(aiRouteExplainCmd, []string{"hello"}); err != nil {
			t.Fatalf("route-explain: %v", err)
		}
	})
	for _, want := range []string{"winner: anthropic-work/claude-sonnet-4-5", "candidates:", "matched route", "false", "512", "0.100000", "caller/month/1.250000@route"} {
		if !strings.Contains(out, want) {
			t.Fatalf("route explain output missing %q: %s", want, out)
		}
	}
}

func TestAICmdChatRequestFile(t *testing.T) {
	_ = newAIFixture(t)

	path := filepath.Join(t.TempDir(), "request.yaml")
	req := `
operation: chat
provider_hint: anthropic-work
input:
  - role: system
    parts:
      - type: text
        text: be concise
  - role: user
    parts:
      - type: text
        text: explain the route
`
	if err := os.WriteFile(path, []byte(req), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	aiRequestFileFlag = path

	stdout, stderr := captureStdStreams(t, func() {
		if err := aiChatCmd.RunE(aiChatCmd, nil); err != nil {
			t.Fatalf("chat request-file: %v", err)
		}
	})
	if !strings.Contains(stdout, "hello from tether") {
		t.Fatalf("stdout missing assistant text: %q", stdout)
	}
	if !strings.Contains(stderr, "provider=anthropic-work") {
		t.Fatalf("stderr missing response metadata: %q", stderr)
	}
}

func TestAICmdUsagePretty(t *testing.T) {
	_ = newAIFixture(t)
	aiProviderFlag = "anthropic-work"

	out := captureStdout(t, func() {
		if err := aiUsageCmd.RunE(aiUsageCmd, nil); err != nil {
			t.Fatalf("usage: %v", err)
		}
	})
	for _, want := range []string{"requests: 2", "estimated_cost_usd: 0.006200", "by_provider:", "anthropic-work"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage output missing %q: %s", want, out)
		}
	}
}

func TestAICmdBudgetsPretty(t *testing.T) {
	_ = newAIFixture(t)
	aiCallerIDFlag = "agent-1"

	out := captureStdout(t, func() {
		if err := aiBudgetsCmd.RunE(aiBudgetsCmd, nil); err != nil {
			t.Fatalf("budgets: %v", err)
		}
	})
	for _, want := range []string{"anthropic-work", "caller/month/1.250000@route", "0.006200", "1.243800", "false", "count: 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("budgets output missing %q: %s", want, out)
		}
	}
}

func TestAICmdAuditRejectsBadSince(t *testing.T) {
	_ = newAIFixture(t)
	aiSinceFlag = "not-a-timestamp"

	err := aiAuditCmd.RunE(aiAuditCmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var ee *exitErr
	if !errorsAs(err, &ee) || ee.code != 2 {
		t.Fatalf("err=%v want exit code 2", err)
	}
}

func TestAICmdAuditPretty(t *testing.T) {
	_ = newAIFixture(t)

	out := captureStdout(t, func() {
		if err := aiAuditCmd.RunE(aiAuditCmd, nil); err != nil {
			t.Fatalf("audit: %v", err)
		}
	})
	for _, want := range []string{"budget_rejection", "anthropic-work", "usage budget 1.250000 USD/month exceeded: spent 1.300000 USD", "count: 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q: %s", want, out)
		}
	}
}

func TestAICmdWatchBudgetsPretty(t *testing.T) {
	t.Cleanup(resetAIFlags)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/stream" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query()["scope"]; len(got) != 1 || got[0] != "daemon" {
			t.Fatalf("scope query = %v", got)
		}
		if got := r.URL.Query()["kind"]; len(got) != 1 || got[0] != events.KindAIBudgetRejected {
			t.Fatalf("kind query = %v", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"id: 7\n" +
				"event: " + events.KindAIBudgetRejected + "\n" +
				`data: {"scope":"daemon","payload_json":"{\"provider\":\"anthropic-work\",\"model\":\"claude-sonnet-4-5\",\"caller_id\":\"agent-1\",\"error\":\"usage budget exceeded\"}"}` + "\n\n",
		))
	}))
	defer srv.Close()

	prevFactory := aiClientFactory
	aiClientFactory = func() (*client.Client, error) {
		return client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://")), nil
	}
	t.Cleanup(func() { aiClientFactory = prevFactory })

	aiCallerIDFlag = "agent-1"
	out := captureStdout(t, func() {
		if err := aiWatchBudgetsCmd.RunE(aiWatchBudgetsCmd, nil); err != nil {
			t.Fatalf("watch-budgets: %v", err)
		}
	})
	for _, want := range []string{"7", "anthropic-work", "claude-sonnet-4-5", "agent-1", "usage budget exceeded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("watch output missing %q: %s", want, out)
		}
	}
}

func captureStdStreams(t *testing.T, fn func()) (string, string) {
	t.Helper()
	oldOut := os.Stdout
	oldErr := os.Stderr

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}

	os.Stdout = outW
	os.Stderr = errW

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() {
		_, _ = outBuf.ReadFrom(outR)
		close(outDone)
	}()
	go func() {
		_, _ = errBuf.ReadFrom(errR)
		close(errDone)
	}()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	<-outDone
	<-errDone
	os.Stdout = oldOut
	os.Stderr = oldErr
	return outBuf.String(), errBuf.String()
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
	if err := emit(llm.StreamEvent{
		Kind:       llm.StreamEventCompleted,
		Provider:   s.streamResp.Provider,
		Model:      s.streamResp.Model,
		StopReason: s.streamResp.StopReason,
		Usage:      s.streamResp.Usage,
		Response:   &s.streamResp,
	}); err != nil {
		return llm.Response{}, err
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
