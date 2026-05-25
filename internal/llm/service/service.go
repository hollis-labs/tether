package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/observability"
	"github.com/hollis-labs/tether/internal/llm/router"
	"github.com/hollis-labs/tether/internal/llm/usagebudget"
)

var (
	ErrProviderNotRegistered = errors.New("llm provider not registered")
	ErrUnsupportedOperation  = errors.New("llm operation not supported")
)

// RoutePlanner describes the routing dependency the AI service needs.
type RoutePlanner interface {
	Plan(req llm.Request) (router.Plan, error)
	Explain(req llm.Request) router.Explanation
}

// Catalog provides model metadata for introspection surfaces.
type Catalog interface {
	List() []modelsdev.ModelRef
}

// ProviderInfo is the non-secret configured provider shape exposed by the AI
// read-side surfaces.
type ProviderInfo struct {
	ID           string
	Type         string
	DefaultModel string
	Models       []string
	BaseURL      string
}

// Service is the first in-process AI gateway surface. It owns route planning,
// middleware execution, and dispatch into a concrete provider adapter.
type Service struct {
	Planner      RoutePlanner
	Catalog      Catalog
	Providers    map[string]llm.ChatProvider
	ProviderInfo map[string]ProviderInfo
	Routes       []router.Route
	RouteOrder   []string
	Middleware   []llm.Middleware
	Recorder     observability.Recorder
	Publisher    events.Publisher
}

// Chat routes one normalized chat request through middleware and into the
// selected provider adapter.
func (s *Service) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	if req.Operation != llm.OperationChat {
		return llm.Response{}, fmt.Errorf("%w: %s", ErrUnsupportedOperation, req.Operation)
	}
	if s.Planner == nil {
		return llm.Response{}, fmt.Errorf("llm service planner is nil")
	}

	handler := llm.BuildMiddlewareChain(s.handleChat, s.Middleware)
	start := time.Now()
	resp, err := handler(ctx, req)
	if err != nil {
		s.recordBudgetRejections(req, s.Planner.Explain(req), time.Since(start))
	}
	s.recordAuditEvent("chat", req, router.Plan{
		Provider:         resp.Route.Provider,
		Model:            resp.Route.Model,
		EstimatedCostUSD: resp.Usage.EstimatedCostUSD,
		Reasons:          append([]string(nil), resp.Route.Reasons...),
		PolicyVersion:    resp.Route.PolicyVersion,
	}, resp, err, time.Since(start))
	return resp, err
}

// StreamChat routes one normalized chat request through the planner and a
// streaming-capable provider adapter, emitting normalized incremental events.
// When the provider lacks native streaming support, this falls back to a unary
// chat invocation and emits only start/completed events.
func (s *Service) StreamChat(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error) {
	if req.Operation != llm.OperationChat {
		return llm.Response{}, fmt.Errorf("%w: %s", ErrUnsupportedOperation, req.Operation)
	}
	if s.Planner == nil {
		return llm.Response{}, fmt.Errorf("llm service planner is nil")
	}
	start := time.Now()
	resp, err := s.streamChat(ctx, req, emit)
	if err != nil {
		s.recordBudgetRejections(req, s.Planner.Explain(req), time.Since(start))
	}
	s.recordAuditEvent("chat", req, router.Plan{
		Provider:         resp.Route.Provider,
		Model:            resp.Route.Model,
		EstimatedCostUSD: resp.Usage.EstimatedCostUSD,
		Reasons:          append([]string(nil), resp.Route.Reasons...),
		PolicyVersion:    resp.Route.PolicyVersion,
	}, resp, err, time.Since(start))
	return resp, err
}

// PreviewRoute runs route planning without invoking a provider adapter.
func (s *Service) PreviewRoute(req llm.Request) (router.Plan, error) {
	if req.Operation == "" {
		req.Operation = llm.OperationChat
	}
	if req.Operation != llm.OperationChat {
		return router.Plan{}, fmt.Errorf("%w: %s", ErrUnsupportedOperation, req.Operation)
	}
	if s.Planner == nil {
		return router.Plan{}, fmt.Errorf("llm service planner is nil")
	}
	start := time.Now()
	plan, err := s.Planner.Plan(req)
	if err != nil {
		s.recordBudgetRejections(req, s.Planner.Explain(req), time.Since(start))
	}
	s.recordAuditEvent("route_preview", req, plan, llm.Response{}, err, time.Since(start))
	return plan, err
}

// ExplainRoute returns a structured planner trace for one request.
func (s *Service) ExplainRoute(req llm.Request) (router.Explanation, error) {
	if req.Operation == "" {
		req.Operation = llm.OperationChat
	}
	if req.Operation != llm.OperationChat {
		return router.Explanation{}, fmt.Errorf("%w: %s", ErrUnsupportedOperation, req.Operation)
	}
	if s.Planner == nil {
		return router.Explanation{}, fmt.Errorf("llm service planner is nil")
	}
	start := time.Now()
	explanation := s.Planner.Explain(req)
	var plan router.Plan
	if explanation.Winner != nil {
		plan = *explanation.Winner
	}
	callErr := error(nil)
	if explanation.Error != "" {
		callErr = errors.New(explanation.Error)
	}
	s.recordBudgetRejections(req, explanation, time.Since(start))
	s.recordAuditEvent("route_explain", req, plan, llm.Response{}, callErr, time.Since(start))
	return explanation, nil
}

// ListProviders returns configured AI providers in stable route order.
func (s *Service) ListProviders() []ProviderInfo {
	if len(s.ProviderInfo) == 0 {
		return nil
	}
	out := make([]ProviderInfo, 0, len(s.ProviderInfo))
	seen := map[string]struct{}{}
	for _, id := range s.RouteOrder {
		info, ok := s.ProviderInfo[id]
		if !ok {
			continue
		}
		out = append(out, info)
		seen[id] = struct{}{}
	}
	var extras []ProviderInfo
	for id, info := range s.ProviderInfo {
		if _, ok := seen[id]; ok {
			continue
		}
		extras = append(extras, info)
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].ID < extras[j].ID })
	return append(out, extras...)
}

// ListRoutes returns the mounted planner routes in stable order.
func (s *Service) ListRoutes() []router.Route {
	if len(s.Routes) == 0 {
		return nil
	}
	return append([]router.Route(nil), s.Routes...)
}

// ListModels returns configured models from the shared catalog. When
// providerID is non-empty, only that configured provider's model set is
// returned.
func (s *Service) ListModels(providerID string) []modelsdev.ModelRef {
	if s.Catalog == nil || len(s.ProviderInfo) == 0 {
		return nil
	}
	refs := s.Catalog.List()
	out := make([]modelsdev.ModelRef, 0, len(refs))
	allowed := map[string]map[string]struct{}{}
	for _, info := range s.ListProviders() {
		if providerID != "" && info.ID != providerID {
			continue
		}
		models := info.Models
		if len(models) == 0 && info.DefaultModel != "" {
			models = []string{info.DefaultModel}
		}
		if len(models) == 0 {
			continue
		}
		set := make(map[string]struct{}, len(models))
		for _, model := range models {
			set[model] = struct{}{}
		}
		allowed[info.ID] = set
	}
	for _, ref := range refs {
		for _, info := range s.ListProviders() {
			models, ok := allowed[info.ID]
			if !ok {
				continue
			}
			if matchesVendor(info.Type, ref.ProviderID) {
				if _, ok := models[ref.ID]; !ok {
					continue
				}
				out = append(out, ref)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Service) handleChat(ctx context.Context, req llm.Request) (llm.Response, error) {
	plan, err := s.Planner.Plan(req)
	if err != nil {
		return llm.Response{}, err
	}

	provider, ok := s.Providers[plan.Provider]
	if !ok {
		return llm.Response{}, fmt.Errorf("%w: %s", ErrProviderNotRegistered, plan.Provider)
	}

	route := plan.RouteDecision()
	resp, err := provider.Chat(ctx, req, route)
	if err != nil {
		return llm.Response{}, err
	}
	if resp.Provider == "" {
		resp.Provider = route.Provider
	}
	if resp.Model == "" {
		resp.Model = route.Model
	}
	resp.Route = route
	if resp.Usage.EstimatedCostUSD == 0 && plan.EstimatedCostUSD > 0 {
		resp.Usage.EstimatedCostUSD = plan.EstimatedCostUSD
	}
	return resp, nil
}

func (s *Service) streamChat(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error) {
	plan, err := s.Planner.Plan(req)
	if err != nil {
		return llm.Response{}, err
	}

	provider, ok := s.Providers[plan.Provider]
	if !ok {
		return llm.Response{}, fmt.Errorf("%w: %s", ErrProviderNotRegistered, plan.Provider)
	}

	route := plan.RouteDecision()
	if emit != nil {
		if err := emit(llm.StreamEvent{
			Kind:     llm.StreamEventStart,
			Provider: route.Provider,
			Model:    route.Model,
		}); err != nil {
			return llm.Response{}, err
		}
	}

	var resp llm.Response
	if streamer, ok := provider.(llm.StreamChatProvider); ok {
		resp, err = streamer.StreamChat(ctx, req, route, emit)
	} else {
		resp, err = provider.Chat(ctx, req, route)
	}
	if err != nil {
		return llm.Response{}, err
	}
	if resp.Provider == "" {
		resp.Provider = route.Provider
	}
	if resp.Model == "" {
		resp.Model = route.Model
	}
	resp.Route = route
	if resp.Usage.EstimatedCostUSD == 0 && plan.EstimatedCostUSD > 0 {
		resp.Usage.EstimatedCostUSD = plan.EstimatedCostUSD
	}
	if emit != nil {
		if err := emit(llm.StreamEvent{
			Kind:       llm.StreamEventCompleted,
			Provider:   resp.Provider,
			Model:      resp.Model,
			StopReason: resp.StopReason,
			Usage:      resp.Usage,
			Response:   &resp,
		}); err != nil {
			return llm.Response{}, err
		}
	}
	return resp, nil
}

func matchesVendor(providerType, vendorID string) bool {
	switch providerType {
	case "anthropic":
		return vendorID == "anthropic"
	case "openai", "openai-compatible":
		return vendorID == "openai"
	default:
		return false
	}
}

func (s *Service) recordAuditEvent(eventType string, req llm.Request, plan router.Plan, resp llm.Response, callErr error, latency time.Duration) {
	if s.Recorder == nil {
		return
	}
	ev := observability.AuditEvent{
		EventType:        eventType,
		RequestID:        req.RequestID,
		SessionID:        req.SessionID,
		CallerID:         req.CallerID,
		Operation:        string(req.Operation),
		Provider:         firstNonEmpty(resp.Provider, plan.Provider),
		Model:            firstNonEmpty(resp.Model, plan.Model),
		PolicyVersion:    firstNonEmpty(resp.Route.PolicyVersion, plan.PolicyVersion),
		LatencyMs:        latency.Milliseconds(),
		Success:          callErr == nil,
		Refusal:          resp.Refusal,
		InputTokens:      resp.Usage.InputTokens,
		OutputTokens:     resp.Usage.OutputTokens,
		CacheReadTokens:  resp.Usage.CacheReadTokens,
		CacheWriteTokens: resp.Usage.CacheWriteTokens,
		ReasoningTokens:  resp.Usage.ReasoningTokens,
		EstimatedCostUSD: maxFloat(resp.Usage.EstimatedCostUSD, plan.EstimatedCostUSD),
		RequestSummary:   summarizeRequest(req),
		ResponseSummary:  summarizeResponse(resp),
		Timestamp:        time.Now().UTC(),
	}
	if callErr != nil {
		ev.Error = callErr.Error()
	}
	_ = s.Recorder.RecordAIAuditEvent(ev)
}

func (s *Service) recordBudgetRejections(req llm.Request, explanation router.Explanation, latency time.Duration) {
	if s.Recorder == nil && s.Publisher == nil {
		return
	}
	for _, candidate := range explanation.Candidates {
		if !usagebudget.IsRejectionMessage(candidate.Error) {
			continue
		}
		ev := observability.AuditEvent{
			EventType:      "budget_rejection",
			RequestID:      req.RequestID,
			SessionID:      req.SessionID,
			CallerID:       req.CallerID,
			Operation:      string(req.Operation),
			Provider:       candidate.Route.Provider,
			Model:          candidate.Route.Model,
			PolicyVersion:  explanation.PolicyVersion,
			LatencyMs:      latency.Milliseconds(),
			Success:        false,
			Error:          candidate.Error,
			RequestSummary: summarizeRequest(req),
			Timestamp:      time.Now().UTC(),
		}
		if s.Recorder != nil {
			_ = s.Recorder.RecordAIAuditEvent(ev)
		}
		s.publishBudgetRejection(req, explanation.PolicyVersion, candidate)
	}
}

func (s *Service) publishBudgetRejection(req llm.Request, policyVersion string, candidate router.CandidateExplanation) {
	if s.Publisher == nil {
		return
	}
	payload, err := json.Marshal(struct {
		RequestID     string `json:"request_id,omitempty"`
		SessionID     string `json:"session_id,omitempty"`
		CallerID      string `json:"caller_id,omitempty"`
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		PolicyVersion string `json:"policy_version,omitempty"`
		Error         string `json:"error"`
	}{
		RequestID:     req.RequestID,
		SessionID:     req.SessionID,
		CallerID:      req.CallerID,
		Provider:      candidate.Route.Provider,
		Model:         candidate.Route.Model,
		PolicyVersion: policyVersion,
		Error:         candidate.Error,
	})
	if err != nil {
		return
	}
	_ = s.Publisher.Publish(context.Background(), events.Event{
		Scope:       events.ScopeDaemon,
		Kind:        events.KindAIBudgetRejected,
		PayloadJSON: string(payload),
	})
}

func summarizeRequest(req llm.Request) string {
	return fmt.Sprintf("messages=%d tools=%d attachments=%d streaming=%t provider_hint=%q model_hint=%q mode=%q intent=%q",
		len(req.Input), len(req.Tools), len(req.Attachments), req.Streaming, req.ProviderHint, req.ModelHint, req.Mode, req.Intent)
}

func summarizeResponse(resp llm.Response) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("messages=%d stop_reason=%q", len(resp.Output), resp.StopReason))
	if resp.Refusal != "" {
		parts = append(parts, "refusal=true")
	}
	var toolUses int
	for _, msg := range resp.Output {
		if msg.ToolUse != nil {
			toolUses++
		}
	}
	if toolUses > 0 {
		parts = append(parts, fmt.Sprintf("tool_uses=%d", toolUses))
	}
	return strings.Join(parts, " ")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
