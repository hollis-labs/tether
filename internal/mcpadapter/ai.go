package mcpadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	neturl "net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/llm"
)

func (a *Adapter) registerAITools(s *server.MCPServer) {
	a.mcp = s
	a.addTool(s,
		mcp.NewTool("mux_ai_list_providers",
			mcp.WithDescription("List configured AI gateway providers exposed by the running muxd daemon."),
		),
		Reads("configured provider listing from the catalog"), a.handleAIProviders,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_list_models",
			mcp.WithDescription("List AI models visible through configured providers on the running muxd daemon."),
			mcp.WithString("provider_id",
				mcp.Description("Optional configured provider id filter"),
			),
		),
		Reads("configured model listing from the catalog"), a.handleAIModels,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_list_routes",
			mcp.WithDescription("List the configured AI planner routes exposed by the running muxd daemon."),
		),
		Reads("configured route listing from the catalog"), a.handleAIRoutes,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_route_preview",
			mcp.WithDescription("Preview which provider/model the AI gateway would route a chat request to, without invoking a model."),
			mcp.WithString("text", mcp.Description("Primary user message text for the shorthand request form")),
			mcp.WithObject("request",
				mcp.Description("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			),
			mcp.WithString("request_json", mcp.Description("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request.")),
			mcp.WithString("system_prompt", mcp.Description("Optional system prompt")),
			mcp.WithArray("image_urls", mcp.Description("Optional image URLs to append as user content parts"), mcp.WithStringItems()),
			mcp.WithString("image_base64", mcp.Description("Optional inline image bytes as base64 for the shorthand request form")),
			mcp.WithString("image_mime_type", mcp.Description("MIME type for image_base64, e.g. image/png")),
			mcp.WithString("provider", mcp.Description("Configured provider id hint")),
			mcp.WithString("model", mcp.Description("Model hint")),
			mcp.WithString("mode", mcp.Description("Mode hint, such as summarize or tool-heavy")),
			mcp.WithString("intent", mcp.Description("Intent hint")),
			mcp.WithString("request_id", mcp.Description("Optional request correlation id")),
			mcp.WithString("session_id", mcp.Description("Optional session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Optional caller correlation id")),
			mcp.WithNumber("max_output_tokens", mcp.Description("Optional max output tokens hint")),
			mcp.WithNumber("token_budget", mcp.Description("Optional token budget hint")),
			mcp.WithNumber("cost_budget_usd", mcp.Description("Optional cost budget hint in USD")),
			mcp.WithNumber("latency_target_ms", mcp.Description("Optional latency target in milliseconds")),
		),
		Reads("route resolution is computed and persisted nowhere"), a.handleAIRoutePreview,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_route_explain",
			mcp.WithDescription("Explain why each configured AI route matched, failed, or was skipped for a normalized chat request."),
			mcp.WithString("text", mcp.Description("Primary user message text for the shorthand request form")),
			mcp.WithObject("request",
				mcp.Description("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			),
			mcp.WithString("request_json", mcp.Description("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request.")),
			mcp.WithString("system_prompt", mcp.Description("Optional system prompt")),
			mcp.WithArray("image_urls", mcp.Description("Optional image URLs to append as user content parts"), mcp.WithStringItems()),
			mcp.WithString("image_base64", mcp.Description("Optional inline image bytes as base64 for the shorthand request form")),
			mcp.WithString("image_mime_type", mcp.Description("MIME type for image_base64, e.g. image/png")),
			mcp.WithString("provider", mcp.Description("Configured provider id hint")),
			mcp.WithString("model", mcp.Description("Model hint")),
			mcp.WithString("mode", mcp.Description("Mode hint, such as summarize or tool-heavy")),
			mcp.WithString("intent", mcp.Description("Intent hint")),
			mcp.WithString("request_id", mcp.Description("Optional request correlation id")),
			mcp.WithString("session_id", mcp.Description("Optional session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Optional caller correlation id")),
			mcp.WithNumber("max_output_tokens", mcp.Description("Optional max output tokens hint")),
			mcp.WithNumber("token_budget", mcp.Description("Optional token budget hint")),
			mcp.WithNumber("cost_budget_usd", mcp.Description("Optional cost budget hint in USD")),
			mcp.WithNumber("latency_target_ms", mcp.Description("Optional latency target in milliseconds")),
		),
		Reads("route resolution is computed and persisted nowhere"), a.handleAIRouteExplain,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_chat",
			mcp.WithDescription("Invoke the AI gateway with a simple normalized chat request and return the final normalized response. Requires the ai.invoke scope."),
			mcp.WithString("text", mcp.Description("Primary user message text for the shorthand request form")),
			mcp.WithObject("request",
				mcp.Description("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			),
			mcp.WithString("request_json", mcp.Description("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request.")),
			mcp.WithString("system_prompt", mcp.Description("Optional system prompt")),
			mcp.WithArray("image_urls", mcp.Description("Optional image URLs to append as user content parts"), mcp.WithStringItems()),
			mcp.WithString("image_base64", mcp.Description("Optional inline image bytes as base64 for the shorthand request form")),
			mcp.WithString("image_mime_type", mcp.Description("MIME type for image_base64, e.g. image/png")),
			mcp.WithString("provider", mcp.Description("Configured provider id hint")),
			mcp.WithString("model", mcp.Description("Model hint")),
			mcp.WithString("mode", mcp.Description("Mode hint, such as summarize or tool-heavy")),
			mcp.WithString("intent", mcp.Description("Intent hint")),
			mcp.WithString("request_id", mcp.Description("Optional request correlation id")),
			mcp.WithString("session_id", mcp.Description("Optional session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Optional caller correlation id")),
			mcp.WithNumber("max_output_tokens", mcp.Description("Optional max output tokens hint")),
			mcp.WithNumber("token_budget", mcp.Description("Optional token budget hint")),
			mcp.WithNumber("cost_budget_usd", mcp.Description("Optional cost budget hint in USD")),
			mcp.WithNumber("latency_target_ms", mcp.Description("Optional latency target in milliseconds")),
		),
		Writes().OpenWorld(), a.handleAIChat,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_embeddings",
			mcp.WithDescription("Generate embedding vectors through the AI gateway. Requires the ai.invoke scope."),
			mcp.WithString("text", mcp.Description("Input text for the shorthand embedding request form")),
			mcp.WithObject("request",
				mcp.Description("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			),
			mcp.WithString("request_json", mcp.Description("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request.")),
			mcp.WithString("provider", mcp.Description("Configured provider id hint")),
			mcp.WithString("model", mcp.Description("Model hint")),
			mcp.WithString("mode", mcp.Description("Mode hint")),
			mcp.WithString("intent", mcp.Description("Intent hint")),
			mcp.WithString("request_id", mcp.Description("Optional request correlation id")),
			mcp.WithString("session_id", mcp.Description("Optional session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Optional caller correlation id")),
			mcp.WithNumber("token_budget", mcp.Description("Optional token budget hint")),
			mcp.WithNumber("cost_budget_usd", mcp.Description("Optional cost budget hint in USD")),
			mcp.WithNumber("latency_target_ms", mcp.Description("Optional latency target in milliseconds")),
		),
		Writes().OpenWorld(), a.handleAIEmbeddings,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_chat_stream",
			mcp.WithDescription("Invoke the AI gateway as a live stream. Emits MCP notifications for incremental stream events and returns the final normalized response. Requires the ai.invoke scope."),
			mcp.WithString("text", mcp.Description("Primary user message text for the shorthand request form")),
			mcp.WithObject("request",
				mcp.Description("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			),
			mcp.WithString("request_json", mcp.Description("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request.")),
			mcp.WithString("system_prompt", mcp.Description("Optional system prompt")),
			mcp.WithArray("image_urls", mcp.Description("Optional image URLs to append as user content parts"), mcp.WithStringItems()),
			mcp.WithString("image_base64", mcp.Description("Optional inline image bytes as base64 for the shorthand request form")),
			mcp.WithString("image_mime_type", mcp.Description("MIME type for image_base64, e.g. image/png")),
			mcp.WithString("provider", mcp.Description("Configured provider id hint")),
			mcp.WithString("model", mcp.Description("Model hint")),
			mcp.WithString("mode", mcp.Description("Mode hint, such as summarize or tool-heavy")),
			mcp.WithString("intent", mcp.Description("Intent hint")),
			mcp.WithString("request_id", mcp.Description("Optional request correlation id")),
			mcp.WithString("session_id", mcp.Description("Optional session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Optional caller correlation id")),
			mcp.WithNumber("max_output_tokens", mcp.Description("Optional max output tokens hint")),
			mcp.WithNumber("token_budget", mcp.Description("Optional token budget hint")),
			mcp.WithNumber("cost_budget_usd", mcp.Description("Optional cost budget hint in USD")),
			mcp.WithNumber("latency_target_ms", mcp.Description("Optional latency target in milliseconds")),
		),
		Writes().OpenWorld(), a.handleAIChatStream,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_usage",
			mcp.WithDescription("Query durable AI usage aggregates recorded by the running muxd daemon."),
			mcp.WithString("provider", mcp.Description("Filter by configured provider id")),
			mcp.WithString("model", mcp.Description("Filter by model id")),
			mcp.WithString("session_id", mcp.Description("Filter by session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Filter by caller correlation id")),
			mcp.WithString("operation", mcp.Description("Filter by operation kind, default chat")),
			mcp.WithString("since", mcp.Description("RFC3339 lower-bound timestamp")),
		),
		Reads("GET /ai/usage"), a.handleAIUsage,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_budgets",
			mcp.WithDescription("List live durable AI usage budgets and current spend for routed providers/models."),
			mcp.WithString("provider", mcp.Description("Filter by configured provider id")),
			mcp.WithString("model", mcp.Description("Filter by model id")),
			mcp.WithString("session_id", mcp.Description("Session id used for session-scoped budgets")),
			mcp.WithString("caller_id", mcp.Description("Caller id used for caller-scoped budgets")),
		),
		Reads("GET /ai/budgets"), a.handleAIBudgets,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_audit",
			mcp.WithDescription("Query durable sanitized AI audit events recorded by the running muxd daemon."),
			mcp.WithString("event_type", mcp.Description("Filter by event type, e.g. chat or route_preview")),
			mcp.WithString("provider", mcp.Description("Filter by configured provider id")),
			mcp.WithString("model", mcp.Description("Filter by model id")),
			mcp.WithString("session_id", mcp.Description("Filter by session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Filter by caller correlation id")),
			mcp.WithString("since", mcp.Description("RFC3339 lower-bound timestamp")),
			mcp.WithNumber("limit", mcp.Description("Max rows to return (default 100, max 2000)")),
			mcp.WithBoolean("errors_only", mcp.Description("When true, only return failed events")),
		),
		Reads("GET /ai/audit"), a.handleAIAudit,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_budget_alerts",
			mcp.WithDescription("Query durable AI budget_rejection audit events recorded by the running muxd daemon."),
			mcp.WithString("provider", mcp.Description("Filter by configured provider id")),
			mcp.WithString("model", mcp.Description("Filter by model id")),
			mcp.WithString("session_id", mcp.Description("Filter by session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Filter by caller correlation id")),
			mcp.WithString("since", mcp.Description("RFC3339 lower-bound timestamp")),
			mcp.WithNumber("limit", mcp.Description("Max rows to return (default 100, max 2000)")),
		),
		Reads("budget alert listing"), a.handleAIBudgetAlerts,
	)
	a.addTool(s,
		mcp.NewTool("mux_ai_wait_budget_alerts",
			mcp.WithDescription("Wait briefly for live ai.budget_rejected daemon events and return any matching alerts."),
			mcp.WithString("provider", mcp.Description("Filter by configured provider id")),
			mcp.WithString("model", mcp.Description("Filter by model id")),
			mcp.WithString("session_id", mcp.Description("Filter by session correlation id")),
			mcp.WithString("caller_id", mcp.Description("Filter by caller correlation id")),
			mcp.WithNumber("since_seq", mcp.Description("Only return events with seq greater than this value")),
			mcp.WithNumber("wait_ms", mcp.Description("Maximum time to wait for events in milliseconds (default 5000)")),
			mcp.WithNumber("max_events", mcp.Description("Maximum matching events to return before stopping (default 1)")),
		),
		Reads("blocks on an alert it does not cause"), a.handleAIWaitBudgetAlerts,
	)
}

func (a *Adapter) handleAIProviders(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	out, err := c.AIProviders(ctx)
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":        true,
		"providers": out.Providers,
		"count":     len(out.Providers),
	}), nil
}

func (a *Adapter) handleAIModels(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	out, err := c.AIModels(ctx, str(req, "provider_id"))
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"models": out.Models,
		"count":  len(out.Models),
	}), nil
}

func (a *Adapter) handleAIRoutes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	out, err := c.AIRoutes(ctx)
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"routes": out.Routes,
		"count":  len(out.Routes),
	}), nil
}

func (a *Adapter) handleAIRoutePreview(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	request, errTool := aiRequestFromTool(req)
	if errTool != nil {
		return errTool, nil
	}
	out, err := c.AIPreviewRoute(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"route": out.Route,
	}), nil
}

func (a *Adapter) handleAIRouteExplain(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	request, errTool := aiRequestFromTool(req)
	if errTool != nil {
		return errTool, nil
	}
	out, err := c.AIExplainRoute(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":         true,
		"winner":     out.Winner,
		"policy":     out.PolicyVersion,
		"error":      out.Error,
		"candidates": out.Candidates,
		"count":      len(out.Candidates),
	}), nil
}

func (a *Adapter) handleAIChat(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeAIInvoke); denied != nil {
		return denied, nil
	}
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	request, errTool := aiRequestFromTool(req)
	if errTool != nil {
		return errTool, nil
	}
	out, err := c.AIChat(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":       true,
		"response": out.Response,
	}), nil
}

func (a *Adapter) handleAIEmbeddings(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeAIInvoke); denied != nil {
		return denied, nil
	}
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	request, errTool := aiEmbeddingRequestFromTool(req)
	if errTool != nil {
		return errTool, nil
	}
	out, err := c.AIEmbeddings(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":       true,
		"response": out.Response,
	}), nil
}

func (a *Adapter) handleAIChatStream(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeAIInvoke); denied != nil {
		return denied, nil
	}
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	request, errTool := aiRequestFromTool(req)
	if errTool != nil {
		return errTool, nil
	}
	request.Streaming = true

	stream, errCh, err := c.AIChatStream(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}

	progressToken := any(nil)
	if req.Params.Meta != nil {
		progressToken = req.Params.Meta.ProgressToken
	}

	var (
		eventCount int
		finalResp  *llm.Response
		lastErr    string
	)
	for stream != nil || errCh != nil {
		select {
		case ev, ok := <-stream:
			if !ok {
				stream = nil
				continue
			}
			eventCount++
			a.emitAIStreamNotification(ctx, progressToken, eventCount, ev)
			if ev.Kind == llm.StreamEventCompleted && ev.Response != nil {
				resp := *ev.Response
				finalResp = &resp
			}
			if ev.Kind == llm.StreamEventError && ev.Error != "" {
				lastErr = ev.Error
			}
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				return toolError("internal_error", err.Error()), nil
			}
		case <-ctx.Done():
			return toolError("internal_error", ctx.Err().Error()), nil
		}
	}
	if finalResp == nil {
		if lastErr != "" {
			return toolError("internal_error", lastErr), nil
		}
		return toolError("internal_error", "stream completed without final response"), nil
	}
	return toolJSON(map[string]any{
		"ok":          true,
		"response":    finalResp,
		"event_count": eventCount,
		"streamed":    true,
	}), nil
}

func (a *Adapter) handleAIUsage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	out, err := c.AIUsage(ctx, client.AIUsageQuery{
		Provider:  str(req, "provider"),
		Model:     str(req, "model"),
		SessionID: str(req, "session_id"),
		CallerID:  str(req, "caller_id"),
		Operation: str(req, "operation"),
		Since:     str(req, "since"),
	})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":      true,
		"summary": out,
	}), nil
}

func (a *Adapter) handleAIBudgets(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	out, err := c.AIBudgets(ctx, client.AIBudgetsQuery{
		Provider:  str(req, "provider"),
		Model:     str(req, "model"),
		SessionID: str(req, "session_id"),
		CallerID:  str(req, "caller_id"),
	})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":      true,
		"budgets": out.Budgets,
		"count":   out.Count,
	}), nil
}

func (a *Adapter) handleAIAudit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	out, err := c.AIAudit(ctx, client.AIAuditQuery{
		EventType:  str(req, "event_type"),
		Provider:   str(req, "provider"),
		Model:      str(req, "model"),
		SessionID:  str(req, "session_id"),
		CallerID:   str(req, "caller_id"),
		Limit:      intArg(req, "limit", 100),
		Since:      str(req, "since"),
		ErrorsOnly: boolArg(req, "errors_only"),
	})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"events": out.Events,
		"count":  out.Count,
	}), nil
}

func (a *Adapter) handleAIBudgetAlerts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	out, err := c.AIAudit(ctx, client.AIAuditQuery{
		EventType: "budget_rejection",
		Provider:  str(req, "provider"),
		Model:     str(req, "model"),
		SessionID: str(req, "session_id"),
		CallerID:  str(req, "caller_id"),
		Since:     str(req, "since"),
		Limit:     intArg(req, "limit", 0),
	})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"alerts": out.Events,
		"count":  out.Count,
	}), nil
}

func (a *Adapter) handleAIWaitBudgetAlerts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	c, errRes := a.requireAIClient()
	if errRes != nil {
		return errRes, nil
	}
	waitMS := intArg(req, "wait_ms", 5000)
	if waitMS <= 0 {
		waitMS = 5000
	}
	maxEvents := intArg(req, "max_events", 1)
	if maxEvents <= 0 {
		maxEvents = 1
	}
	streamCtx, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
	defer cancel()

	stream, errCh, err := c.StreamEvents(streamCtx, client.EventsStreamQuery{
		SinceSeq: int64(intArg(req, "since_seq", 0)),
		Scopes:   []string{events.ScopeDaemon},
		Kinds:    []string{events.KindAIBudgetRejected},
	})
	if err != nil {
		return a.classifyAIClientErr(err), nil
	}

	var (
		alerts  []map[string]any
		lastSeq int64
	)
	for len(alerts) < maxEvents {
		select {
		case ev, ok := <-stream:
			if !ok {
				stream = nil
				if len(alerts) >= maxEvents {
					break
				}
				goto done
			}
			if ev.Seq > lastSeq {
				lastSeq = ev.Seq
			}
			payload, err := decodeAIBudgetAlertPayload(ev.PayloadJSON)
			if err != nil {
				return toolError("internal_error", err.Error()), nil
			}
			if !matchesAIBudgetAlertFilters(req, payload) {
				continue
			}
			alerts = append(alerts, map[string]any{
				"seq":            ev.Seq,
				"provider":       payload.Provider,
				"model":          payload.Model,
				"request_id":     payload.RequestID,
				"session_id":     payload.SessionID,
				"caller_id":      payload.CallerID,
				"policy_version": payload.PolicyVersion,
				"error":          payload.Error,
			})
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				return toolError("internal_error", err.Error()), nil
			}
			goto done
		case <-streamCtx.Done():
			goto done
		}
	}

done:
	timedOut := errors.Is(streamCtx.Err(), context.DeadlineExceeded)
	return toolJSON(map[string]any{
		"ok":             true,
		"alerts":         alerts,
		"count":          len(alerts),
		"timed_out":      timedOut,
		"next_since_seq": lastSeq,
	}), nil
}

func (a *Adapter) emitAIStreamNotification(ctx context.Context, progressToken any, eventCount int, ev llm.StreamEvent) {
	if a.mcp == nil {
		return
	}
	payload := map[string]any{
		"source": "mux_ai_chat_stream",
		"event":  ev,
	}
	_ = a.mcp.SendLogMessageToClient(ctx, mcp.NewLoggingMessageNotification(mcp.LoggingLevelInfo, "tether.ai.stream", payload))
	_ = a.mcp.SendNotificationToClient(ctx, "notifications/ai/chat_stream", payload)
	if progressToken != nil {
		progress := float64(eventCount)
		params := map[string]any{
			"progressToken": progressToken,
			"progress":      progress,
			"message":       string(ev.Kind),
		}
		if ev.Kind == llm.StreamEventCompleted {
			params["message"] = "response.completed"
		}
		_ = a.mcp.SendNotificationToClient(ctx, "notifications/progress", params)
	}
}

type aiBudgetAlertPayload struct {
	RequestID     string `json:"request_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	CallerID      string `json:"caller_id,omitempty"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	PolicyVersion string `json:"policy_version,omitempty"`
	Error         string `json:"error"`
}

func decodeAIBudgetAlertPayload(raw string) (aiBudgetAlertPayload, error) {
	var out aiBudgetAlertPayload
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return aiBudgetAlertPayload{}, err
	}
	return out, nil
}

func matchesAIBudgetAlertFilters(req mcp.CallToolRequest, payload aiBudgetAlertPayload) bool {
	if v := str(req, "provider"); v != "" && payload.Provider != v {
		return false
	}
	if v := str(req, "model"); v != "" && payload.Model != v {
		return false
	}
	if v := str(req, "session_id"); v != "" && payload.SessionID != v {
		return false
	}
	if v := str(req, "caller_id"); v != "" && payload.CallerID != v {
		return false
	}
	return true
}

func aiRequestFromTool(req mcp.CallToolRequest) (llm.Request, *mcp.CallToolResult) {
	request, errRes := decodeAIRequestOverride(req)
	if errRes != nil {
		return llm.Request{}, errRes
	}
	if request.Operation == "" {
		request.Operation = llm.OperationChat
	}
	request = applyAIRequestToolOverrides(req, request)
	request, errRes = applyAIRequestToolImages(req, request)
	if errRes != nil {
		return llm.Request{}, errRes
	}
	if len(request.Input) == 0 {
		return llm.Request{}, toolError("invalid_request", "request input required")
	}
	return request, nil
}

func aiEmbeddingRequestFromTool(req mcp.CallToolRequest) (llm.Request, *mcp.CallToolResult) {
	request, errRes := decodeAIRequestOverride(req)
	if errRes != nil {
		return llm.Request{}, errRes
	}
	if request.Operation == "" {
		request.Operation = llm.OperationEmbedding
	}
	if text := str(req, "text"); text != "" && len(request.EmbeddingInput) == 0 {
		request.EmbeddingInput = []string{text}
		request.Input = nil
	}
	request = applyAIRequestToolOverrides(req, request)
	if request.Operation != llm.OperationEmbedding {
		return llm.Request{}, toolError("invalid_request", "request operation must be embedding")
	}
	if len(request.EmbeddingInput) == 0 {
		return llm.Request{}, toolError("invalid_request", "embedding_input required")
	}
	return request, nil
}

func decodeAIRequestOverride(req mcp.CallToolRequest) (llm.Request, *mcp.CallToolResult) {
	args := req.GetArguments()
	text := str(req, "text")
	rawRequest, hasRequest := args["request"]
	requestJSON := str(req, "request_json")

	if hasRequest && requestJSON != "" {
		return llm.Request{}, toolError("invalid_request", "request and request_json are mutually exclusive")
	}
	if (hasRequest || requestJSON != "") && text != "" {
		return llm.Request{}, toolError("invalid_request", "text cannot be combined with request or request_json")
	}
	if hasRequest {
		b, err := json.Marshal(rawRequest)
		if err != nil {
			return llm.Request{}, toolError("invalid_request", "request must be a valid object")
		}
		var request llm.Request
		if err := json.Unmarshal(b, &request); err != nil {
			return llm.Request{}, toolError("invalid_request", "request must decode as llm.Request: "+err.Error())
		}
		return request, nil
	}
	if requestJSON != "" {
		var request llm.Request
		if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
			return llm.Request{}, toolError("invalid_request", "request_json must decode as llm.Request: "+err.Error())
		}
		return request, nil
	}
	if text == "" && len(strSliceArg(req, "image_urls")) == 0 && str(req, "image_base64") == "" {
		return llm.Request{}, toolError("invalid_request", "text, image_urls, image_base64, request, or request_json required")
	}
	request := llm.Request{Operation: llm.OperationChat}
	if text != "" {
		request.Input = []llm.Message{{
			Role:  "user",
			Parts: []llm.ContentPart{{Type: "text", Text: text}},
		}}
	}
	return request, nil
}

func applyAIRequestToolOverrides(req mcp.CallToolRequest, request llm.Request) llm.Request {
	if v := str(req, "provider"); v != "" {
		request.ProviderHint = v
	}
	if v := str(req, "model"); v != "" {
		request.ModelHint = v
	}
	if v := str(req, "mode"); v != "" {
		request.Mode = v
	}
	if v := str(req, "intent"); v != "" {
		request.Intent = v
	}
	if v := str(req, "request_id"); v != "" {
		request.RequestID = v
	}
	if v := str(req, "session_id"); v != "" {
		request.SessionID = v
	}
	if v := str(req, "caller_id"); v != "" {
		request.CallerID = v
	}
	if v := intArg(req, "max_output_tokens", 0); v > 0 {
		request.MaxOutputTokens = v
	}
	if v := intArg(req, "token_budget", 0); v > 0 {
		request.TokenBudget = v
	}
	if v := floatArg(req, "cost_budget_usd", 0); v > 0 {
		request.CostBudgetUSD = v
	}
	if v := intArg(req, "latency_target_ms", 0); v > 0 {
		request.LatencyTargetMS = v
	}
	if systemPrompt := str(req, "system_prompt"); systemPrompt != "" {
		request.Input = append([]llm.Message{{
			Role:  "system",
			Parts: []llm.ContentPart{{Type: "text", Text: systemPrompt}},
		}}, request.Input...)
	}
	return request
}

func applyAIRequestToolImages(req mcp.CallToolRequest, request llm.Request) (llm.Request, *mcp.CallToolResult) {
	parts := make([]llm.ContentPart, 0, len(strSliceArg(req, "image_urls"))+1)
	for _, rawURL := range strSliceArg(req, "image_urls") {
		part, err := imagePartFromToolURL(rawURL)
		if err != nil {
			return llm.Request{}, toolError("invalid_request", err.Error())
		}
		parts = append(parts, part)
	}
	if rawBase64 := str(req, "image_base64"); rawBase64 != "" {
		part, err := imagePartFromToolBase64(rawBase64, str(req, "image_mime_type"))
		if err != nil {
			return llm.Request{}, toolError("invalid_request", err.Error())
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return request, nil
	}
	request.Input = append(request.Input, llm.Message{
		Role:  "user",
		Parts: parts,
	})
	return request, nil
}

func imagePartFromToolURL(rawURL string) (llm.ContentPart, error) {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return llm.ContentPart{}, fmt.Errorf("invalid image url %q: %w", rawURL, err)
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(parsed.Path)))
	if !strings.HasPrefix(mimeType, "image/") {
		return llm.ContentPart{}, fmt.Errorf("image url %q requires an image-like extension so mime type can be inferred", rawURL)
	}
	return llm.ContentPart{
		Type:     "image",
		MIMEType: mimeType,
		URL:      rawURL,
		Name:     filepath.Base(parsed.Path),
	}, nil
}

func imagePartFromToolBase64(rawBase64, mimeType string) (llm.ContentPart, error) {
	mimeType = strings.TrimSpace(mimeType)
	if !strings.HasPrefix(mimeType, "image/") {
		return llm.ContentPart{}, fmt.Errorf("image_mime_type is required for image_base64 and must start with image/")
	}
	data, err := base64.StdEncoding.DecodeString(rawBase64)
	if err != nil {
		return llm.ContentPart{}, fmt.Errorf("image_base64 must be valid base64: %w", err)
	}
	return llm.ContentPart{
		Type:     "image",
		MIMEType: mimeType,
		Data:     data,
	}, nil
}

func (a *Adapter) requireAIClient() (*client.Client, *mcp.CallToolResult) {
	if a.client == nil {
		return nil, toolError("daemon_unavailable", "AI tools require muxd daemon routing; start muxd and run mux mcp against that catalog")
	}
	return a.client, nil
}

func (a *Adapter) classifyAIClientErr(err error) *mcp.CallToolResult {
	if isDaemonUnreachable(err) {
		return daemonUnreachableError(err)
	}
	return classifyClientErr(err, "")
}
