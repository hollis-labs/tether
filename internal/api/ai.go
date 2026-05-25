package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/llm"
	llmanthropic "github.com/hollis-labs/tether/internal/llm/anthropic"
	"github.com/hollis-labs/tether/internal/llm/router"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
	"github.com/hollis-labs/tether/internal/llm/usagebudget"
	"github.com/hollis-labs/tether/internal/store"
)

// maxAIRequestBytes caps POST /ai/chat body size.
const maxAIRequestBytes = 1 << 20

type ChatRequest struct {
	Request llm.Request `json:"request"`
}

type ChatResponse struct {
	Response llm.Response `json:"response"`
}

type RoutePreviewResponse struct {
	Route RoutePreview `json:"route"`
}

type RouteExplainResponse struct {
	PolicyVersion string                    `json:"policy_version,omitempty"`
	Winner        *RoutePreview             `json:"winner,omitempty"`
	Error         string                    `json:"error,omitempty"`
	Candidates    []AIRouteExplainCandidate `json:"candidates"`
}

type RoutePreview struct {
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	EstimatedCostUSD float64  `json:"estimated_cost_usd,omitempty"`
	Reasons          []string `json:"reasons,omitempty"`
	PolicyVersion    string   `json:"policy_version,omitempty"`
}

type ListAIProvidersResponse struct {
	Providers []AIProviderDTO `json:"providers"`
}

type AIProviderDTO struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	DefaultModel string   `json:"default_model,omitempty"`
	Models       []string `json:"models,omitempty"`
	BaseURL      string   `json:"base_url,omitempty"`
}

type ListAIModelsResponse struct {
	Models []AIModelDTO `json:"models"`
}

type ListAIRoutesResponse struct {
	Routes []AIRouteDTO `json:"routes"`
}

type AIRouteDTO struct {
	Provider          string                  `json:"provider"`
	Model             string                  `json:"model"`
	Mode              string                  `json:"mode,omitempty"`
	Intent            string                  `json:"intent,omitempty"`
	RequiresReasoning bool                    `json:"requires_reasoning,omitempty"`
	RequiresTools     bool                    `json:"requires_tools,omitempty"`
	AllowReasoning    *bool                   `json:"allow_reasoning,omitempty"`
	AllowTools        *bool                   `json:"allow_tools,omitempty"`
	AllowAttachments  *bool                   `json:"allow_attachments,omitempty"`
	MaxOutputTokens   *int                    `json:"max_output_tokens,omitempty"`
	MaxCostUSD        *float64                `json:"max_cost_usd,omitempty"`
	UsageBudget       *AIUsageBudgetPolicyDTO `json:"usage_budget,omitempty"`
}

type AIRouteExplainCandidate struct {
	Provider          string                  `json:"provider"`
	Model             string                  `json:"model"`
	Mode              string                  `json:"mode,omitempty"`
	Intent            string                  `json:"intent,omitempty"`
	RequiresReasoning bool                    `json:"requires_reasoning,omitempty"`
	RequiresTools     bool                    `json:"requires_tools,omitempty"`
	AllowReasoning    *bool                   `json:"allow_reasoning,omitempty"`
	AllowTools        *bool                   `json:"allow_tools,omitempty"`
	AllowAttachments  *bool                   `json:"allow_attachments,omitempty"`
	MaxOutputTokens   *int                    `json:"max_output_tokens,omitempty"`
	MaxCostUSD        *float64                `json:"max_cost_usd,omitempty"`
	UsageBudget       *AIUsageBudgetPolicyDTO `json:"usage_budget,omitempty"`
	Matched           bool                    `json:"matched"`
	Selected          bool                    `json:"selected,omitempty"`
	Reasons           []string                `json:"reasons,omitempty"`
	Error             string                  `json:"error,omitempty"`
	EstimatedCostUSD  float64                 `json:"estimated_cost_usd,omitempty"`
}

type AIUsageBudgetPolicyDTO struct {
	Level      string   `json:"level,omitempty"`
	MaxCostUSD *float64 `json:"max_cost_usd,omitempty"`
	Window     string   `json:"window,omitempty"`
	Scope      string   `json:"scope,omitempty"`
}

type AIModelDTO struct {
	ConfiguredProviderID string   `json:"configured_provider_id"`
	VendorProviderID     string   `json:"vendor_provider_id"`
	ID                   string   `json:"id"`
	Name                 string   `json:"name,omitempty"`
	Family               string   `json:"family,omitempty"`
	ContextWindow        int      `json:"context_window,omitempty"`
	MaxOutputTokens      int      `json:"max_output_tokens,omitempty"`
	InputModalities      []string `json:"input_modalities,omitempty"`
	OutputModalities     []string `json:"output_modalities,omitempty"`
}

type AIAuditStore interface {
	QueryAIEvents(f store.AIEventFilter) ([]store.AIEvent, error)
}

type AIUsageStore interface {
	QueryAIUsageSummary(f store.AIUsageFilter) (store.AIUsageSummary, error)
}

type AIAuditEventDTO struct {
	ID               int64   `json:"id"`
	EventType        string  `json:"event_type"`
	RequestID        string  `json:"request_id,omitempty"`
	SessionID        string  `json:"session_id,omitempty"`
	CallerID         string  `json:"caller_id,omitempty"`
	Operation        string  `json:"operation"`
	Provider         string  `json:"provider,omitempty"`
	Model            string  `json:"model,omitempty"`
	PolicyVersion    string  `json:"policy_version,omitempty"`
	LatencyMs        int64   `json:"latency_ms"`
	Success          bool    `json:"success"`
	Refusal          string  `json:"refusal,omitempty"`
	Error            string  `json:"error,omitempty"`
	InputTokens      int     `json:"input_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens,omitempty"`
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd,omitempty"`
	RequestSummary   string  `json:"request_summary,omitempty"`
	ResponseSummary  string  `json:"response_summary,omitempty"`
	Timestamp        string  `json:"timestamp"`
}

type AIAuditListResponse struct {
	Events []AIAuditEventDTO `json:"events"`
	Count  int               `json:"count"`
}

type AIUsageBreakdownDTO struct {
	Key              string  `json:"key"`
	Requests         int     `json:"requests"`
	Successes        int     `json:"successes"`
	Errors           int     `json:"errors"`
	LatencyMs        int64   `json:"latency_ms"`
	InputTokens      int     `json:"input_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens,omitempty"`
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd,omitempty"`
}

type AIUsageResponse struct {
	Requests         int                   `json:"requests"`
	Successes        int                   `json:"successes"`
	Errors           int                   `json:"errors"`
	LatencyMs        int64                 `json:"latency_ms"`
	InputTokens      int                   `json:"input_tokens,omitempty"`
	OutputTokens     int                   `json:"output_tokens,omitempty"`
	CacheReadTokens  int                   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int                   `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int                   `json:"reasoning_tokens,omitempty"`
	EstimatedCostUSD float64               `json:"estimated_cost_usd,omitempty"`
	ByProvider       []AIUsageBreakdownDTO `json:"by_provider,omitempty"`
	ByModel          []AIUsageBreakdownDTO `json:"by_model,omitempty"`
	ByOperation      []AIUsageBreakdownDTO `json:"by_operation,omitempty"`
}

type AIUsageBudgetEntryDTO struct {
	Provider         string                 `json:"provider"`
	Model            string                 `json:"model"`
	Mode             string                 `json:"mode,omitempty"`
	Intent           string                 `json:"intent,omitempty"`
	UsageBudget      AIUsageBudgetPolicyDTO `json:"usage_budget"`
	WindowStart      string                 `json:"window_start"`
	SpentCostUSD     float64                `json:"spent_cost_usd,omitempty"`
	RemainingCostUSD float64                `json:"remaining_cost_usd,omitempty"`
	Exhausted        bool                   `json:"exhausted"`
	Filter           AIUsageBudgetFilterDTO `json:"filter"`
	Error            string                 `json:"error,omitempty"`
}

type AIUsageBudgetFilterDTO struct {
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	CallerID  string `json:"caller_id,omitempty"`
	Operation string `json:"operation,omitempty"`
}

type AIUsageBudgetsResponse struct {
	Budgets []AIUsageBudgetEntryDTO `json:"budgets"`
	Count   int                     `json:"count"`
}

func (s *Server) registerAIRoutes(mux *http.ServeMux) {
	if s.AI == nil {
		return
	}
	mux.HandleFunc("/ai/chat", s.handleAIChat)
	mux.HandleFunc("/ai/chat/stream", s.handleAIChatStream)
	mux.HandleFunc("/ai/providers", s.handleAIProviders)
	mux.HandleFunc("/ai/models", s.handleAIModels)
	mux.HandleFunc("/ai/routes", s.handleAIRoutes)
	mux.HandleFunc("/ai/routes/explain", s.handleAIRouteExplain)
	mux.HandleFunc("/ai/routes/preview", s.handleAIRoutePreview)
	if s.AIUsage != nil {
		mux.HandleFunc("/ai/usage", s.handleAIUsage)
		mux.HandleFunc("/ai/budgets", s.handleAIBudgets)
	}
	if s.AIAudit != nil {
		mux.HandleFunc("/ai/audit", s.handleAIAudit)
	}
}

func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	var req ChatRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAIRequestBytes+1))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "decode body: "+err.Error())
		return
	}
	if req.Request.Operation == "" {
		req.Request.Operation = llm.OperationChat
	}
	if req.Request.Operation != llm.OperationChat {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "operation must be chat for /ai/chat")
		return
	}

	resp, err := s.AI.Chat(r.Context(), req.Request)
	if err != nil {
		status, code := aiErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ChatResponse{Response: resp})
}

func (s *Server) handleAIChatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "streaming not supported")
		return
	}

	var req ChatRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAIRequestBytes+1))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "decode body: "+err.Error())
		return
	}
	if req.Request.Operation == "" {
		req.Request.Operation = llm.OperationChat
	}
	if req.Request.Operation != llm.OperationChat {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "operation must be chat for /ai/chat/stream")
		return
	}
	req.Request.Streaming = true

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(ev llm.StreamEvent) error {
		return writeAIStreamEvent(w, flusher, ev)
	}
	if _, err := s.AI.StreamChat(r.Context(), req.Request, emit); err != nil {
		_ = writeAIStreamEvent(w, flusher, llm.StreamEvent{
			Kind:  llm.StreamEventError,
			Error: err.Error(),
		})
	}
}

func writeAIStreamEvent(w http.ResponseWriter, flusher http.Flusher, ev llm.StreamEvent) error {
	var buf strings.Builder
	fmt.Fprintf(&buf, "event: %s\n", ev.Kind)
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	fmt.Fprintf(&buf, "data: %s\n\n", b)
	if _, err := w.Write([]byte(buf.String())); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (s *Server) handleAIProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	providers := s.AI.ListProviders()
	out := make([]AIProviderDTO, 0, len(providers))
	for _, p := range providers {
		out = append(out, AIProviderDTO{
			ID:           p.ID,
			Type:         p.Type,
			DefaultModel: p.DefaultModel,
			Models:       append([]string(nil), p.Models...),
			BaseURL:      p.BaseURL,
		})
	}
	writeJSON(w, http.StatusOK, ListAIProvidersResponse{Providers: out})
}

func (s *Server) handleAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	configured := s.AI.ListProviders()
	refs := s.AI.ListModels(r.URL.Query().Get("provider_id"))
	out := make([]AIModelDTO, 0, len(refs))
	for _, ref := range refs {
		for _, info := range configuredProvidersForVendor(configured, ref.ProviderID) {
			if requested := r.URL.Query().Get("provider_id"); requested != "" && requested != info.ID {
				continue
			}
			if !providerInfoAllowsModel(info, ref.ID) {
				continue
			}
			out = append(out, modelRefDTO(info, ref))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConfiguredProviderID != out[j].ConfiguredProviderID {
			return out[i].ConfiguredProviderID < out[j].ConfiguredProviderID
		}
		return out[i].ID < out[j].ID
	})
	writeJSON(w, http.StatusOK, ListAIModelsResponse{Models: out})
}

func (s *Server) handleAIRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	routes := s.AI.ListRoutes()
	out := make([]AIRouteDTO, 0, len(routes))
	for _, route := range routes {
		out = append(out, AIRouteDTO{
			Provider:          route.Provider,
			Model:             route.Model,
			Mode:              route.Mode,
			Intent:            route.Intent,
			RequiresReasoning: route.RequiresReasoning,
			RequiresTools:     route.RequiresTools,
			AllowReasoning:    route.AllowReasoning,
			AllowTools:        route.AllowTools,
			AllowAttachments:  route.AllowAttachments,
			MaxOutputTokens:   route.MaxOutputTokens,
			MaxCostUSD:        route.MaxCostUSD,
			UsageBudget:       usageBudgetPolicyDTO(route.UsageBudget),
		})
	}
	writeJSON(w, http.StatusOK, ListAIRoutesResponse{Routes: out})
}

func (s *Server) handleAIRoutePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	var req ChatRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAIRequestBytes+1))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "decode body: "+err.Error())
		return
	}
	if req.Request.Operation == "" {
		req.Request.Operation = llm.OperationChat
	}

	plan, err := s.AI.PreviewRoute(req.Request)
	if err != nil {
		status, code := aiErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, RoutePreviewResponse{
		Route: RoutePreview{
			Provider:         plan.Provider,
			Model:            plan.Model,
			EstimatedCostUSD: plan.EstimatedCostUSD,
			Reasons:          append([]string(nil), plan.Reasons...),
			PolicyVersion:    plan.PolicyVersion,
		},
	})
}

func (s *Server) handleAIRouteExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	var req ChatRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAIRequestBytes+1))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "decode body: "+err.Error())
		return
	}
	if req.Request.Operation == "" {
		req.Request.Operation = llm.OperationChat
	}

	explanation, err := s.AI.ExplainRoute(req.Request)
	if err != nil {
		status, code := aiErrorStatus(err)
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, routeExplainResponseDTO(explanation))
}

func aiErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, router.ErrNoCandidates), errors.Is(err, router.ErrNoRouteMatch):
		return http.StatusBadRequest, CodeInvalidRequest
	case errors.Is(err, llmservice.ErrUnsupportedOperation):
		return http.StatusBadRequest, CodeInvalidRequest
	case errors.Is(err, llmanthropic.ErrUnsupportedInput):
		return http.StatusBadRequest, CodeInvalidRequest
	default:
		return http.StatusInternalServerError, CodeInternalError
	}
}

func (s *Server) handleAIAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	filter := store.AIEventFilter{
		EventType: q.Get("event_type"),
		Provider:  q.Get("provider"),
		Model:     q.Get("model"),
		SessionID: q.Get("session_id"),
		CallerID:  q.Get("caller_id"),
	}
	if lim := q.Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil {
			filter.Limit = n
		}
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		}
	}
	if q.Get("errors_only") == "true" {
		filter.ErrorsOnly = true
	}

	rows, err := s.AIAudit.QueryAIEvents(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "query ai audit: "+err.Error())
		return
	}
	out := make([]AIAuditEventDTO, len(rows))
	for i, ev := range rows {
		out[i] = AIAuditEventDTO{
			ID:               ev.ID,
			EventType:        ev.EventType,
			RequestID:        ev.RequestID,
			SessionID:        ev.SessionID,
			CallerID:         ev.CallerID,
			Operation:        ev.Operation,
			Provider:         ev.Provider,
			Model:            ev.Model,
			PolicyVersion:    ev.PolicyVersion,
			LatencyMs:        ev.LatencyMs,
			Success:          ev.Success,
			Refusal:          ev.Refusal,
			Error:            ev.Error,
			InputTokens:      ev.InputTokens,
			OutputTokens:     ev.OutputTokens,
			CacheReadTokens:  ev.CacheReadTokens,
			CacheWriteTokens: ev.CacheWriteTokens,
			ReasoningTokens:  ev.ReasoningTokens,
			EstimatedCostUSD: ev.EstimatedCostUSD,
			RequestSummary:   ev.RequestSummary,
			ResponseSummary:  ev.ResponseSummary,
			Timestamp:        ev.Timestamp.UTC().Format(time.RFC3339Nano),
		}
	}
	writeJSON(w, http.StatusOK, AIAuditListResponse{Events: out, Count: len(out)})
}

func (s *Server) handleAIUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	filter := store.AIUsageFilter{
		Provider:  q.Get("provider"),
		Model:     q.Get("model"),
		SessionID: q.Get("session_id"),
		CallerID:  q.Get("caller_id"),
		Operation: q.Get("operation"),
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		}
	}

	summary, err := s.AIUsage.QueryAIUsageSummary(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "query ai usage: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, AIUsageResponse{
		Requests:         summary.Requests,
		Successes:        summary.Successes,
		Errors:           summary.Errors,
		LatencyMs:        summary.LatencyMs,
		InputTokens:      summary.InputTokens,
		OutputTokens:     summary.OutputTokens,
		CacheReadTokens:  summary.CacheReadTokens,
		CacheWriteTokens: summary.CacheWriteTokens,
		ReasoningTokens:  summary.ReasoningTokens,
		EstimatedCostUSD: summary.EstimatedCostUSD,
		ByProvider:       usageBreakdownDTOs(summary.ByProvider),
		ByModel:          usageBreakdownDTOs(summary.ByModel),
		ByOperation:      usageBreakdownDTOs(summary.ByOperation),
	})
}

func (s *Server) handleAIBudgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if s.AI == nil || s.AIUsage == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "route not found")
		return
	}

	req := llm.Request{
		Operation: llm.OperationChat,
		SessionID: r.URL.Query().Get("session_id"),
		CallerID:  r.URL.Query().Get("caller_id"),
	}
	providerFilter := r.URL.Query().Get("provider")
	modelFilter := r.URL.Query().Get("model")
	routes := s.AI.ListRoutes()
	out := make([]AIUsageBudgetEntryDTO, 0, len(routes))
	for _, route := range routes {
		if route.UsageBudget.MaxCostUSD == nil {
			continue
		}
		if providerFilter != "" && route.Provider != providerFilter {
			continue
		}
		if modelFilter != "" && route.Model != modelFilter {
			continue
		}

		windowStart := usagebudgetWindowStart(nowUTC(), route.UsageBudget.Window)
		entry := AIUsageBudgetEntryDTO{
			Provider:    route.Provider,
			Model:       route.Model,
			Mode:        route.Mode,
			Intent:      route.Intent,
			UsageBudget: *usageBudgetPolicyDTO(route.UsageBudget),
			WindowStart: windowStart.Format(time.RFC3339),
		}
		filter, err := usagebudget.BuildFilter(req, route)
		if err != nil {
			entry.Error = err.Error()
			out = append(out, entry)
			continue
		}
		filter.Since = windowStart
		entry.Filter = AIUsageBudgetFilterDTO{
			Provider:  filter.Provider,
			Model:     filter.Model,
			SessionID: filter.SessionID,
			CallerID:  filter.CallerID,
			Operation: filter.Operation,
		}
		summary, err := s.AIUsage.QueryAIUsageSummary(filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "query ai usage budget: "+err.Error())
			return
		}
		remaining := *route.UsageBudget.MaxCostUSD - summary.EstimatedCostUSD
		if remaining < 0 {
			remaining = 0
		}
		entry.SpentCostUSD = summary.EstimatedCostUSD
		entry.RemainingCostUSD = remaining
		entry.Exhausted = summary.EstimatedCostUSD >= *route.UsageBudget.MaxCostUSD
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, AIUsageBudgetsResponse{Budgets: out, Count: len(out)})
}

func configuredProvidersForVendor(in []llmservice.ProviderInfo, vendorProviderID string) []llmservice.ProviderInfo {
	out := make([]llmservice.ProviderInfo, 0, len(in))
	for _, info := range in {
		switch info.Type {
		case "anthropic":
			if vendorProviderID == "anthropic" {
				out = append(out, info)
			}
		case "openai":
			if vendorProviderID == "openai" {
				out = append(out, info)
			}
		case "openai-compatible":
			if vendorProviderID == "openai" {
				out = append(out, info)
			}
		}
	}
	return out
}

func modelRefDTO(info llmservice.ProviderInfo, ref modelsdev.ModelRef) AIModelDTO {
	return AIModelDTO{
		ConfiguredProviderID: info.ID,
		VendorProviderID:     ref.ProviderID,
		ID:                   ref.ID,
		Name:                 ref.Name,
		Family:               ref.Family,
		ContextWindow:        ref.Limit.ContextWindow,
		MaxOutputTokens:      ref.Limit.MaxOutputTokens,
		InputModalities:      append([]string(nil), ref.Modality.Input...),
		OutputModalities:     append([]string(nil), ref.Modality.Output...),
	}
}

func providerInfoAllowsModel(info llmservice.ProviderInfo, modelID string) bool {
	if len(info.Models) == 0 {
		return info.DefaultModel == "" || info.DefaultModel == modelID
	}
	for _, model := range info.Models {
		if model == modelID {
			return true
		}
	}
	return false
}

func usageBreakdownDTOs(in []store.AIUsageBreakdown) []AIUsageBreakdownDTO {
	out := make([]AIUsageBreakdownDTO, len(in))
	for i, row := range in {
		out[i] = AIUsageBreakdownDTO{
			Key:              row.Key,
			Requests:         row.Requests,
			Successes:        row.Successes,
			Errors:           row.Errors,
			LatencyMs:        row.LatencyMs,
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			CacheReadTokens:  row.CacheReadTokens,
			CacheWriteTokens: row.CacheWriteTokens,
			ReasoningTokens:  row.ReasoningTokens,
			EstimatedCostUSD: row.EstimatedCostUSD,
		}
	}
	return out
}

func usageBudgetPolicyDTO(in router.UsageBudgetPolicy) *AIUsageBudgetPolicyDTO {
	if in.Level == "" && in.MaxCostUSD == nil && in.Window == "" && in.Scope == "" {
		return nil
	}
	return &AIUsageBudgetPolicyDTO{
		Level:      in.Level,
		MaxCostUSD: in.MaxCostUSD,
		Window:     in.Window,
		Scope:      in.Scope,
	}
}

func usagebudgetWindowStart(now time.Time, window string) time.Time {
	now = now.UTC()
	switch window {
	case "day":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	default:
		y, m, _ := now.Date()
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	}
}

func nowUTC() time.Time {
	return time.Now().UTC()
}

func routeExplainResponseDTO(explanation router.Explanation) RouteExplainResponse {
	out := RouteExplainResponse{
		PolicyVersion: explanation.PolicyVersion,
		Error:         explanation.Error,
		Candidates:    make([]AIRouteExplainCandidate, 0, len(explanation.Candidates)),
	}
	if explanation.Winner != nil {
		out.Winner = &RoutePreview{
			Provider:         explanation.Winner.Provider,
			Model:            explanation.Winner.Model,
			EstimatedCostUSD: explanation.Winner.EstimatedCostUSD,
			Reasons:          append([]string(nil), explanation.Winner.Reasons...),
			PolicyVersion:    explanation.Winner.PolicyVersion,
		}
	}
	for _, candidate := range explanation.Candidates {
		out.Candidates = append(out.Candidates, AIRouteExplainCandidate{
			Provider:          candidate.Route.Provider,
			Model:             candidate.Route.Model,
			Mode:              candidate.Route.Mode,
			Intent:            candidate.Route.Intent,
			RequiresReasoning: candidate.Route.RequiresReasoning,
			RequiresTools:     candidate.Route.RequiresTools,
			AllowReasoning:    candidate.Route.AllowReasoning,
			AllowTools:        candidate.Route.AllowTools,
			AllowAttachments:  candidate.Route.AllowAttachments,
			MaxOutputTokens:   candidate.Route.MaxOutputTokens,
			MaxCostUSD:        candidate.Route.MaxCostUSD,
			UsageBudget:       usageBudgetPolicyDTO(candidate.Route.UsageBudget),
			Matched:           candidate.Matched,
			Selected:          candidate.Selected,
			Reasons:           append([]string(nil), candidate.Reasons...),
			Error:             candidate.Error,
			EstimatedCostUSD:  candidate.EstimatedCostUSD,
		})
	}
	return out
}
