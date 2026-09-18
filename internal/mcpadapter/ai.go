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

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/llm"
)

func (a *Adapter) registerAITools(s *gomcp.Server) {
	a.mcp = s
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_list_providers",
		Description: "List configured AI gateway providers exposed by the running muxd daemon.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleAIProviders,
	}, Reads("configured provider listing from the catalog"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_list_models",
		Description: "List AI models visible through configured providers on the running muxd daemon.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"provider_id": strProp("Optional configured provider id filter"),
		}),
		Handler: a.handleAIModels,
	}, Reads("configured model listing from the catalog"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_list_routes",
		Description: "List the configured AI planner routes exposed by the running muxd daemon.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleAIRoutes,
	}, Reads("configured route listing from the catalog"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_route_preview",
		Description: "Preview which provider/model the AI gateway would route a chat request to, without invoking a model.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"text":              strProp("Primary user message text for the shorthand request form"),
			"request":           objProp("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			"request_json":      strProp("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request."),
			"system_prompt":     strProp("Optional system prompt"),
			"image_urls":        strArrProp("Optional image URLs to append as user content parts"),
			"image_base64":      strProp("Optional inline image bytes as base64 for the shorthand request form"),
			"image_mime_type":   strProp("MIME type for image_base64, e.g. image/png"),
			"provider":          strProp("Configured provider id hint"),
			"model":             strProp("Model hint"),
			"mode":              strProp("Mode hint, such as summarize or tool-heavy"),
			"intent":            strProp("Intent hint"),
			"request_id":        strProp("Optional request correlation id"),
			"session_id":        strProp("Optional session correlation id"),
			"caller_id":         strProp("Optional caller correlation id"),
			"max_output_tokens": numProp("Optional max output tokens hint"),
			"token_budget":      numProp("Optional token budget hint"),
			"cost_budget_usd":   numProp("Optional cost budget hint in USD"),
			"latency_target_ms": numProp("Optional latency target in milliseconds"),
		}),
		Handler: a.handleAIRoutePreview,
	}, Reads("route resolution is computed and persisted nowhere"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_route_explain",
		Description: "Explain why each configured AI route matched, failed, or was skipped for a normalized chat request.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"text":              strProp("Primary user message text for the shorthand request form"),
			"request":           objProp("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			"request_json":      strProp("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request."),
			"system_prompt":     strProp("Optional system prompt"),
			"image_urls":        strArrProp("Optional image URLs to append as user content parts"),
			"image_base64":      strProp("Optional inline image bytes as base64 for the shorthand request form"),
			"image_mime_type":   strProp("MIME type for image_base64, e.g. image/png"),
			"provider":          strProp("Configured provider id hint"),
			"model":             strProp("Model hint"),
			"mode":              strProp("Mode hint, such as summarize or tool-heavy"),
			"intent":            strProp("Intent hint"),
			"request_id":        strProp("Optional request correlation id"),
			"session_id":        strProp("Optional session correlation id"),
			"caller_id":         strProp("Optional caller correlation id"),
			"max_output_tokens": numProp("Optional max output tokens hint"),
			"token_budget":      numProp("Optional token budget hint"),
			"cost_budget_usd":   numProp("Optional cost budget hint in USD"),
			"latency_target_ms": numProp("Optional latency target in milliseconds"),
		}),
		Handler: a.handleAIRouteExplain,
	}, Reads("route resolution is computed and persisted nowhere"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_chat",
		Description: "Invoke the AI gateway with a simple normalized chat request and return the final normalized response. Requires the ai.invoke scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"text":              strProp("Primary user message text for the shorthand request form"),
			"request":           objProp("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			"request_json":      strProp("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request."),
			"system_prompt":     strProp("Optional system prompt"),
			"image_urls":        strArrProp("Optional image URLs to append as user content parts"),
			"image_base64":      strProp("Optional inline image bytes as base64 for the shorthand request form"),
			"image_mime_type":   strProp("MIME type for image_base64, e.g. image/png"),
			"provider":          strProp("Configured provider id hint"),
			"model":             strProp("Model hint"),
			"mode":              strProp("Mode hint, such as summarize or tool-heavy"),
			"intent":            strProp("Intent hint"),
			"request_id":        strProp("Optional request correlation id"),
			"session_id":        strProp("Optional session correlation id"),
			"caller_id":         strProp("Optional caller correlation id"),
			"max_output_tokens": numProp("Optional max output tokens hint"),
			"token_budget":      numProp("Optional token budget hint"),
			"cost_budget_usd":   numProp("Optional cost budget hint in USD"),
			"latency_target_ms": numProp("Optional latency target in milliseconds"),
		}),
		Handler: a.handleAIChat,
	}, Writes().OpenWorld())
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_embeddings",
		Description: "Generate embedding vectors through the AI gateway. Requires the ai.invoke scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"text":              strProp("Input text for the shorthand embedding request form"),
			"request":           objProp("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			"request_json":      strProp("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request."),
			"provider":          strProp("Configured provider id hint"),
			"model":             strProp("Model hint"),
			"mode":              strProp("Mode hint"),
			"intent":            strProp("Intent hint"),
			"request_id":        strProp("Optional request correlation id"),
			"session_id":        strProp("Optional session correlation id"),
			"caller_id":         strProp("Optional caller correlation id"),
			"token_budget":      numProp("Optional token budget hint"),
			"cost_budget_usd":   numProp("Optional cost budget hint in USD"),
			"latency_target_ms": numProp("Optional latency target in milliseconds"),
		}),
		Handler: a.handleAIEmbeddings,
	}, Writes().OpenWorld())
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_chat_stream",
		Description: "Invoke the AI gateway as a live stream. Emits MCP notifications for incremental stream events and returns the final normalized response. Requires the ai.invoke scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"text":              strProp("Primary user message text for the shorthand request form"),
			"request":           objProp("Full normalized llm.Request object. Mutually exclusive with text and request_json."),
			"request_json":      strProp("Full normalized llm.Request encoded as JSON. Mutually exclusive with text and request."),
			"system_prompt":     strProp("Optional system prompt"),
			"image_urls":        strArrProp("Optional image URLs to append as user content parts"),
			"image_base64":      strProp("Optional inline image bytes as base64 for the shorthand request form"),
			"image_mime_type":   strProp("MIME type for image_base64, e.g. image/png"),
			"provider":          strProp("Configured provider id hint"),
			"model":             strProp("Model hint"),
			"mode":              strProp("Mode hint, such as summarize or tool-heavy"),
			"intent":            strProp("Intent hint"),
			"request_id":        strProp("Optional request correlation id"),
			"session_id":        strProp("Optional session correlation id"),
			"caller_id":         strProp("Optional caller correlation id"),
			"max_output_tokens": numProp("Optional max output tokens hint"),
			"token_budget":      numProp("Optional token budget hint"),
			"cost_budget_usd":   numProp("Optional cost budget hint in USD"),
			"latency_target_ms": numProp("Optional latency target in milliseconds"),
		}),
		Handler: a.handleAIChatStream,
	}, Writes().OpenWorld())
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_usage",
		Description: "Query durable AI usage aggregates recorded by the running muxd daemon.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"provider":   strProp("Filter by configured provider id"),
			"model":      strProp("Filter by model id"),
			"session_id": strProp("Filter by session correlation id"),
			"caller_id":  strProp("Filter by caller correlation id"),
			"operation":  strProp("Filter by operation kind, default chat"),
			"since":      strProp("RFC3339 lower-bound timestamp"),
		}),
		Handler: a.handleAIUsage,
	}, Reads("GET /ai/usage"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_budgets",
		Description: "List live durable AI usage budgets and current spend for routed providers/models.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"provider":   strProp("Filter by configured provider id"),
			"model":      strProp("Filter by model id"),
			"session_id": strProp("Session id used for session-scoped budgets"),
			"caller_id":  strProp("Caller id used for caller-scoped budgets"),
		}),
		Handler: a.handleAIBudgets,
	}, Reads("GET /ai/budgets"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_audit",
		Description: "Query durable sanitized AI audit events recorded by the running muxd daemon.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"event_type":  strProp("Filter by event type, e.g. chat or route_preview"),
			"provider":    strProp("Filter by configured provider id"),
			"model":       strProp("Filter by model id"),
			"session_id":  strProp("Filter by session correlation id"),
			"caller_id":   strProp("Filter by caller correlation id"),
			"since":       strProp("RFC3339 lower-bound timestamp"),
			"limit":       numProp("Max rows to return (default 100, max 2000)"),
			"errors_only": boolProp("When true, only return failed events"),
		}),
		Handler: a.handleAIAudit,
	}, Reads("GET /ai/audit"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_budget_alerts",
		Description: "Query durable AI budget_rejection audit events recorded by the running muxd daemon.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"provider":   strProp("Filter by configured provider id"),
			"model":      strProp("Filter by model id"),
			"session_id": strProp("Filter by session correlation id"),
			"caller_id":  strProp("Filter by caller correlation id"),
			"since":      strProp("RFC3339 lower-bound timestamp"),
			"limit":      numProp("Max rows to return (default 100, max 2000)"),
		}),
		Handler: a.handleAIBudgetAlerts,
	}, Reads("budget alert listing"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_ai_wait_budget_alerts",
		Description: "Wait briefly for live ai.budget_rejected daemon events and return any matching alerts.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"provider":   strProp("Filter by configured provider id"),
			"model":      strProp("Filter by model id"),
			"session_id": strProp("Filter by session correlation id"),
			"caller_id":  strProp("Filter by caller correlation id"),
			"since_seq":  numProp("Only return events with seq greater than this value"),
			"wait_ms":    numProp("Maximum time to wait for events in milliseconds (default 5000)"),
			"max_events": numProp("Maximum matching events to return before stopping (default 1)"),
		}),
		Handler: a.handleAIWaitBudgetAlerts,
	}, Reads("blocks on an alert it does not cause"))
}

func (a *Adapter) handleAIProviders(ctx context.Context, _ map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	out, err := c.AIProviders(ctx)
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":        true,
		"providers": out.Providers,
		"count":     len(out.Providers),
	}), nil
}

func (a *Adapter) handleAIModels(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	out, err := c.AIModels(ctx, str(args, "provider_id"))
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"models": out.Models,
		"count":  len(out.Models),
	}), nil
}

func (a *Adapter) handleAIRoutes(ctx context.Context, _ map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	out, err := c.AIRoutes(ctx)
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"routes": out.Routes,
		"count":  len(out.Routes),
	}), nil
}

func (a *Adapter) handleAIRoutePreview(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	request, err := aiRequestFromTool(args)
	if err != nil {
		return nil, err
	}
	out, err := c.AIPreviewRoute(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"route": out.Route,
	}), nil
}

func (a *Adapter) handleAIRouteExplain(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	request, err := aiRequestFromTool(args)
	if err != nil {
		return nil, err
	}
	out, err := c.AIExplainRoute(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
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

func (a *Adapter) handleAIChat(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeAIInvoke); err != nil {
		return nil, err
	}
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	request, err := aiRequestFromTool(args)
	if err != nil {
		return nil, err
	}
	out, err := c.AIChat(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":       true,
		"response": out.Response,
	}), nil
}

func (a *Adapter) handleAIEmbeddings(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeAIInvoke); err != nil {
		return nil, err
	}
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	request, err := aiEmbeddingRequestFromTool(args)
	if err != nil {
		return nil, err
	}
	out, err := c.AIEmbeddings(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":       true,
		"response": out.Response,
	}), nil
}

func (a *Adapter) handleAIChatStream(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeAIInvoke); err != nil {
		return nil, err
	}
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	request, err := aiRequestFromTool(args)
	if err != nil {
		return nil, err
	}
	request.Streaming = true

	stream, errCh, err := c.AIChatStream(ctx, api.ChatRequest{Request: request})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}

	progressToken := gomcp.MetaFromContext(ctx)["progressToken"]

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
				return nil, toolError("internal_error", err.Error())
			}
		case <-ctx.Done():
			return nil, toolError("internal_error", ctx.Err().Error())
		}
	}
	if finalResp == nil {
		if lastErr != "" {
			return nil, toolError("internal_error", lastErr)
		}
		return nil, toolError("internal_error", "stream completed without final response")
	}
	return toolJSON(map[string]any{
		"ok":          true,
		"response":    finalResp,
		"event_count": eventCount,
		"streamed":    true,
	}), nil
}

func (a *Adapter) handleAIUsage(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	out, err := c.AIUsage(ctx, client.AIUsageQuery{
		Provider:  str(args, "provider"),
		Model:     str(args, "model"),
		SessionID: str(args, "session_id"),
		CallerID:  str(args, "caller_id"),
		Operation: str(args, "operation"),
		Since:     str(args, "since"),
	})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":      true,
		"summary": out,
	}), nil
}

func (a *Adapter) handleAIBudgets(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	out, err := c.AIBudgets(ctx, client.AIBudgetsQuery{
		Provider:  str(args, "provider"),
		Model:     str(args, "model"),
		SessionID: str(args, "session_id"),
		CallerID:  str(args, "caller_id"),
	})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":      true,
		"budgets": out.Budgets,
		"count":   out.Count,
	}), nil
}

func (a *Adapter) handleAIAudit(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	out, err := c.AIAudit(ctx, client.AIAuditQuery{
		EventType:  str(args, "event_type"),
		Provider:   str(args, "provider"),
		Model:      str(args, "model"),
		SessionID:  str(args, "session_id"),
		CallerID:   str(args, "caller_id"),
		Limit:      intArg(args, "limit", 100),
		Since:      str(args, "since"),
		ErrorsOnly: boolArg(args, "errors_only"),
	})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"events": out.Events,
		"count":  out.Count,
	}), nil
}

func (a *Adapter) handleAIBudgetAlerts(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	out, err := c.AIAudit(ctx, client.AIAuditQuery{
		EventType: "budget_rejection",
		Provider:  str(args, "provider"),
		Model:     str(args, "model"),
		SessionID: str(args, "session_id"),
		CallerID:  str(args, "caller_id"),
		Since:     str(args, "since"),
		Limit:     intArg(args, "limit", 0),
	})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
	}
	return toolJSON(map[string]any{
		"ok":     true,
		"alerts": out.Events,
		"count":  out.Count,
	}), nil
}

func (a *Adapter) handleAIWaitBudgetAlerts(ctx context.Context, args map[string]any) (any, error) {
	c, err := a.requireAIClient()
	if err != nil {
		return nil, err
	}
	waitMS := intArg(args, "wait_ms", 5000)
	if waitMS <= 0 {
		waitMS = 5000
	}
	maxEvents := intArg(args, "max_events", 1)
	if maxEvents <= 0 {
		maxEvents = 1
	}
	streamCtx, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
	defer cancel()

	stream, errCh, err := c.StreamEvents(streamCtx, client.EventsStreamQuery{
		SinceSeq: int64(intArg(args, "since_seq", 0)),
		Scopes:   []string{events.ScopeDaemon},
		Kinds:    []string{events.KindAIBudgetRejected},
	})
	if err != nil {
		return nil, a.classifyAIClientErr(err)
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
				return nil, toolError("internal_error", err.Error())
			}
			if !matchesAIBudgetAlertFilters(args, payload) {
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
				return nil, toolError("internal_error", err.Error())
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

// emitAIStreamNotification sends stream progress to the calling client via
// go-mcp's context-installed Notifier (see gomcp.WithNotifier/Notify),
// bridged by go-mcp's server package to the underlying MCP session
// automatically for every tool call -- there is no per-client send method on
// *gomcp.Server itself (go-mcp does not wrap session lifecycle; see
// Adapter.newBareServer's doc comment). Notify is a safe no-op when ctx
// carries no notifier (e.g. a direct in-process CallTool invocation in a
// test), matching the original's `a.mcp == nil` guard.
func (a *Adapter) emitAIStreamNotification(ctx context.Context, progressToken any, eventCount int, ev llm.StreamEvent) {
	payload := map[string]any{
		"source": "mux_ai_chat_stream",
		"event":  ev,
	}
	// notifications/message: go-mcp's session bridge reads params["level"]
	// and forwards params["message"] verbatim as the LoggingMessageParams
	// Data field, so the structured payload rides through as-is. The
	// mark3labs logger-name field ("tether.ai.stream") has no equivalent in
	// go-mcp's bridge and is dropped.
	gomcp.Notify(ctx, gomcp.Notification{
		Method: "notifications/message",
		Params: map[string]any{"level": "info", "message": payload},
	})
	gomcp.Notify(ctx, gomcp.Notification{
		Method: "notifications/ai/chat_stream",
		Params: payload,
	})
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
		gomcp.Notify(ctx, gomcp.Notification{
			Method: "notifications/progress",
			Params: params,
		})
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

func matchesAIBudgetAlertFilters(args map[string]any, payload aiBudgetAlertPayload) bool {
	if v := str(args, "provider"); v != "" && payload.Provider != v {
		return false
	}
	if v := str(args, "model"); v != "" && payload.Model != v {
		return false
	}
	if v := str(args, "session_id"); v != "" && payload.SessionID != v {
		return false
	}
	if v := str(args, "caller_id"); v != "" && payload.CallerID != v {
		return false
	}
	return true
}

func aiRequestFromTool(args map[string]any) (llm.Request, error) {
	request, err := decodeAIRequestOverride(args)
	if err != nil {
		return llm.Request{}, err
	}
	if request.Operation == "" {
		request.Operation = llm.OperationChat
	}
	request = applyAIRequestToolOverrides(args, request)
	request, err = applyAIRequestToolImages(args, request)
	if err != nil {
		return llm.Request{}, err
	}
	if len(request.Input) == 0 {
		return llm.Request{}, toolError("invalid_request", "request input required")
	}
	return request, nil
}

func aiEmbeddingRequestFromTool(args map[string]any) (llm.Request, error) {
	request, err := decodeAIRequestOverride(args)
	if err != nil {
		return llm.Request{}, err
	}
	if request.Operation == "" {
		request.Operation = llm.OperationEmbedding
	}
	if text := str(args, "text"); text != "" && len(request.EmbeddingInput) == 0 {
		request.EmbeddingInput = []string{text}
		request.Input = nil
	}
	request = applyAIRequestToolOverrides(args, request)
	if request.Operation != llm.OperationEmbedding {
		return llm.Request{}, toolError("invalid_request", "request operation must be embedding")
	}
	if len(request.EmbeddingInput) == 0 {
		return llm.Request{}, toolError("invalid_request", "embedding_input required")
	}
	return request, nil
}

func decodeAIRequestOverride(args map[string]any) (llm.Request, error) {
	text := str(args, "text")
	rawRequest, hasRequest := args["request"]
	requestJSON := str(args, "request_json")

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
	if text == "" && len(strSliceArg(args, "image_urls")) == 0 && str(args, "image_base64") == "" {
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

func applyAIRequestToolOverrides(args map[string]any, request llm.Request) llm.Request {
	if v := str(args, "provider"); v != "" {
		request.ProviderHint = v
	}
	if v := str(args, "model"); v != "" {
		request.ModelHint = v
	}
	if v := str(args, "mode"); v != "" {
		request.Mode = v
	}
	if v := str(args, "intent"); v != "" {
		request.Intent = v
	}
	if v := str(args, "request_id"); v != "" {
		request.RequestID = v
	}
	if v := str(args, "session_id"); v != "" {
		request.SessionID = v
	}
	if v := str(args, "caller_id"); v != "" {
		request.CallerID = v
	}
	if v := intArg(args, "max_output_tokens", 0); v > 0 {
		request.MaxOutputTokens = v
	}
	if v := intArg(args, "token_budget", 0); v > 0 {
		request.TokenBudget = v
	}
	if v := floatArg(args, "cost_budget_usd", 0); v > 0 {
		request.CostBudgetUSD = v
	}
	if v := intArg(args, "latency_target_ms", 0); v > 0 {
		request.LatencyTargetMS = v
	}
	if systemPrompt := str(args, "system_prompt"); systemPrompt != "" {
		request.Input = append([]llm.Message{{
			Role:  "system",
			Parts: []llm.ContentPart{{Type: "text", Text: systemPrompt}},
		}}, request.Input...)
	}
	return request
}

func applyAIRequestToolImages(args map[string]any, request llm.Request) (llm.Request, error) {
	parts := make([]llm.ContentPart, 0, len(strSliceArg(args, "image_urls"))+1)
	for _, rawURL := range strSliceArg(args, "image_urls") {
		part, err := imagePartFromToolURL(rawURL)
		if err != nil {
			return llm.Request{}, toolError("invalid_request", err.Error())
		}
		parts = append(parts, part)
	}
	if rawBase64 := str(args, "image_base64"); rawBase64 != "" {
		part, err := imagePartFromToolBase64(rawBase64, str(args, "image_mime_type"))
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

func (a *Adapter) requireAIClient() (*client.Client, error) {
	if a.client == nil {
		return nil, toolError("daemon_unavailable", "AI tools require muxd daemon routing; start muxd and run mux mcp against that catalog")
	}
	return a.client, nil
}

func (a *Adapter) classifyAIClientErr(err error) error {
	if isDaemonUnreachable(err) {
		return daemonUnreachableError(err)
	}
	return classifyClientErr(err, "")
}
