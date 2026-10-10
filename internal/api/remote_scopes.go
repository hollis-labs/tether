package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
)

const (
	readScope     = identity.ScopeRead
	operateScope  = identity.ScopeOperate
	terminalScope = identity.ScopeTerminal
	maintainScope = identity.ScopeMaintain
	adminScope    = identity.ScopeAdmin
	localScope    = "local-only"
	publicScope   = "public"
	selfScope     = "current-device"
)

type RouteScope struct {
	Registration  string
	Path          string
	Methods       map[string]string
	PayloadPolicy string
	PayloadScopes map[string]string
}

func routeScope(registration, path string, methods map[string]string, payloadPolicy ...string) RouteScope {
	rule := RouteScope{Registration: registration, Path: path, Methods: methods}
	if len(payloadPolicy) != 0 {
		rule.PayloadPolicy = payloadPolicy[0]
	}
	return rule
}

// RemoteRouteScopes is the one declarative remote route policy (ADR 0062).
// There is no GET/read or mutation/operate fallback. Unlisted paths, methods
// and payload cases stay refused, including when an optional module appears.
// Registration links item/action policies to their actual ServeMux owner for
// the structural coverage test; it is not a route-count or document check.
var RemoteRouteScopes = []RouteScope{
	routeScope("/environments", "/environments", map[string]string{"GET": readScope, "POST": localScope}),
	routeScope("/environments/", "/environments/{environmentId}", map[string]string{"GET": readScope, "PATCH": localScope, "DELETE": localScope}),
	routeScope("/health", "/health", map[string]string{"GET": publicScope, "HEAD": publicScope}),
	routeScope(environment.DescriptorPath, environment.DescriptorPath, map[string]string{"GET": publicScope, "HEAD": publicScope}),
	routeScope("/v1/environment/report", "/v1/environment/report", map[string]string{"GET": readScope}),
	routeScope("/auth/pair", "/auth/pair", map[string]string{"POST": localScope}),
	routeScope("/auth/pair/revoke", "/auth/pair/revoke", map[string]string{"POST": localScope}),
	routeScope(identity.PairingExchangePath, identity.PairingExchangePath, map[string]string{"POST": publicScope}),
	routeScope("/auth/devices", "/auth/devices", map[string]string{"GET": adminScope}),
	routeScope("/auth/revoke", "/auth/revoke", map[string]string{"POST": adminScope}),
	routeScope("/auth/renew", "/auth/renew", map[string]string{"POST": selfScope}),
	routeScope("/auth/context", "/auth/context", map[string]string{"GET": readScope}),
	routeScope("/sessions", "/sessions", map[string]string{"GET": readScope, "POST": operateScope}, "launch"),
	routeScope("/sessions/", "/sessions/{id}", map[string]string{"GET": readScope}),
	routeScope("/sessions/", "/sessions/{id}/launch", map[string]string{"POST": operateScope}),
	routeScope("/sessions/", "/sessions/{id}/stop", map[string]string{"POST": operateScope}),
	routeScope("/sessions/", "/sessions/{id}/wait", map[string]string{"GET": readScope}),
	routeScope("/sessions/", "/sessions/{id}/input", map[string]string{"POST": terminalScope}),
	routeScope("/sessions/", "/sessions/{id}/turn", map[string]string{"POST": operateScope}),
	routeScope("/sessions/", "/sessions/{id}/resize", map[string]string{"POST": terminalScope}),
	routeScope("/sessions/", "/sessions/{id}/log", map[string]string{"GET": terminalScope}),
	routeScope("/sessions/", "/sessions/{id}/attach", map[string]string{"GET": terminalScope}),
	routeScope("/sessions/", "/sessions/{id}/checkpoint", map[string]string{"POST": operateScope}),
	routeScope("/sessions/", "/sessions/{id}/events", map[string]string{"GET": readScope}),
	routeScope("/sessions/", "/sessions/{id}/attachments", map[string]string{"GET": readScope}),
	routeScope("/sessions/", "/sessions/{id}/checkpoints", map[string]string{"GET": readScope}),
	routeScope("/sessions/", "/sessions/{id}/health", map[string]string{"GET": readScope}),
	routeScope("/sessions/", "/sessions/{id}/workstream", map[string]string{"POST": operateScope}),
	routeScope("/sessions/", "/sessions/{id}/refs", map[string]string{"GET": readScope, "POST": operateScope}),
	routeScope("/sessions/", "/sessions/{id}/workstream-namespace", map[string]string{"GET": readScope}),
	routeScope("/sessions/", "/sessions/{id}/digest", map[string]string{"GET": readScope}),
	routeScope("/sessions/bootstrap", "/sessions/bootstrap", map[string]string{"POST": localScope}),
	routeScope("/sessions/{id}/snapshot", "/sessions/{id}/snapshot", map[string]string{"GET": readScope}),
	routeScope("/sessions/{id}/stream", "/sessions/{id}/stream", map[string]string{"GET": readScope}),
	routeScope("/environment/snapshot", "/environment/snapshot", map[string]string{"GET": readScope}),
	routeScope("/environment/events", "/environment/events", map[string]string{"GET": readScope}),
	routeScope("/logical-agents", "/logical-agents", map[string]string{"GET": readScope}),
	routeScope("/logical-agents/", "/logical-agents/{id}/checkpoints", map[string]string{"GET": readScope}),
	routeScope("/logical-agents/", "/logical-agents/{id}/resume", map[string]string{"POST": operateScope}),
	routeScope("/logical-agents/", "/logical-agents/{id}/policy", map[string]string{"GET": readScope, "PATCH": maintainScope}),
	routeScope("/broker/envelopes", "/broker/envelopes", map[string]string{"GET": readScope, "POST": operateScope}),
	routeScope("/broker/envelopes/", "/broker/envelopes/{id}", map[string]string{"GET": readScope}),
	routeScope("/broker/envelopes/", "/broker/envelopes/{id}/reply", map[string]string{"POST": operateScope}),
	routeScope("/broker/requests", "/broker/requests", map[string]string{"POST": operateScope}),
	routeScope("/session-groups", "/session-groups", map[string]string{"GET": readScope, "POST": operateScope}),
	routeScope("/session-groups/", "/session-groups/{id}", map[string]string{"GET": readScope}),
	routeScope("/session-groups/", "/session-groups/{id}/sessions", map[string]string{"GET": readScope}),
	routeScope("/session-groups/", "/session-groups/{id}/members", map[string]string{"POST": operateScope}),
	routeScope("/workstreams", "/workstreams", map[string]string{"GET": readScope, "POST": operateScope}),
	routeScope("/workstreams/", "/workstreams/{id}", map[string]string{"GET": readScope, "PATCH": operateScope}),
	routeScope("/workstreams/", "/workstreams/{id}/sessions", map[string]string{"POST": operateScope}),
	routeScope("/workstreams/", "/workstreams/{id}/refs", map[string]string{"GET": readScope}),
	routeScope("/workstreams/", "/workstreams/{id}/digest", map[string]string{"GET": readScope}),
	routeScope("/routing/capabilities", "/routing/capabilities", map[string]string{"GET": readScope}),
	routeScope("/channels", "/channels", map[string]string{"GET": readScope}),
	routeScope("/channels/", "/channels/{id}/messages", map[string]string{"GET": readScope}),
	routeScope("/channels/", "/channels/{id}/subscribe", map[string]string{"GET": readScope}),
	routeScope("/messages", "/messages", map[string]string{"POST": operateScope}),
	routeScope("/messages/notify", "/messages/notify", map[string]string{"POST": operateScope}),
	routeScope("/messages/request", "/messages/request", map[string]string{"POST": operateScope}),
	routeScope("/messages/subscribe", "/messages/subscribe", map[string]string{"GET": readScope}),
	routeScope("/messages/inbox", "/messages/inbox", map[string]string{"GET": readScope}),
	routeScope("/messages/list", "/messages/list", map[string]string{"GET": readScope}),
	routeScope("/messages/thread/", "/messages/thread/{id}", map[string]string{"GET": readScope}),
	routeScope("/messages/", "/messages/{id}", map[string]string{"GET": readScope, "DELETE": maintainScope}),
	routeScope("/messages/", "/messages/{id}/consume", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/cancel", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/read", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/archive", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/unarchive", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/claim", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/ack", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/nack", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/reply", map[string]string{"POST": operateScope}),
	routeScope("/messages/", "/messages/{id}/delivery", map[string]string{"GET": readScope}),
	routeScope("/messages/", "/messages/{id}/trace", map[string]string{"GET": readScope}),
	routeScope("/messages/", "/messages/{id}/redrive", map[string]string{"POST": maintainScope}),
	routeScope("/messages/", "/messages/{id}/purge", map[string]string{"POST": maintainScope}),
	routeScope("/messages/retention/candidates", "/messages/retention/candidates", map[string]string{"GET": maintainScope}),
	routeScope("/events", "/events", map[string]string{"GET": readScope}),
	routeScope("/events/stream", "/events/stream", map[string]string{"GET": readScope}),
	routeScope("/events/tool-metrics", "/events/tool-metrics", map[string]string{"GET": readScope}),
	routeScope("/proxy/events", "/proxy/events", map[string]string{"GET": readScope, "POST": localScope}),
	routeScope("/catalog/projects", "/catalog/projects", map[string]string{"GET": readScope}),
	routeScope("/catalog/agents", "/catalog/agents", map[string]string{"GET": readScope}),
	routeScope("/catalog/providers", "/catalog/providers", map[string]string{"GET": readScope}),
	routeScope("/catalog/launches", "/catalog/launches", map[string]string{"GET": readScope}),
	routeScope("/docs/mcp", "/docs/mcp", map[string]string{"GET": readScope}),
	routeScope("/docs/mcp/{id}", "/docs/mcp/{id}", map[string]string{"GET": readScope}),
	routeScope("/docs/mcp/{id}/file", "/docs/mcp/{id}/file", map[string]string{"GET": readScope}),
	routeScope("/registry/bootstrap", "/registry/bootstrap", map[string]string{"POST": maintainScope}),
	routeScope("/registry/reonboard", "/registry/reonboard", map[string]string{"POST": maintainScope}),
	routeScope("/registry/bindings", "/registry/bindings", map[string]string{"GET": readScope, "POST": localScope}),
	routeScope("/registry/bindings/", "/registry/bindings/{id}/renew", map[string]string{"POST": localScope}),
	routeScope("/registry/bindings/", "/registry/bindings/{id}/revoke", map[string]string{"POST": localScope}),
	routeScope("/registry/scoped-bindings", "/registry/scoped-bindings", map[string]string{"POST": maintainScope}),
	routeScope("/registry/scoped-bindings/resolve", "/registry/scoped-bindings/resolve", map[string]string{"GET": readScope}),
	routeScope("/registry/scoped-bindings/revisions", "/registry/scoped-bindings/revisions", map[string]string{"GET": readScope}),
	routeScope("/registry/", "/registry/{kind}", map[string]string{"GET": readScope, "POST": maintainScope}, "registry-kind"),
	routeScope("/registry/", "/registry/{kind}/{id}", map[string]string{"GET": readScope, "PATCH": maintainScope, "DELETE": maintainScope}, "registry-kind"),
	routeScope("/registry/", "/registry/{kind}/{id}/sync", map[string]string{"POST": maintainScope}, "registry-kind"),
	routeScope("/registry/", "/registry/{kind}/{id}/merge", map[string]string{"POST": maintainScope}, "registry-kind"),
	routeScope("/whoami", "/whoami", map[string]string{"GET": readScope}),
	routeScope("/settings/mcp", "/settings/mcp", map[string]string{"GET": readScope, "PUT": maintainScope}),
	routeScope("/settings/onboarding", "/settings/onboarding", map[string]string{"GET": readScope}),
	routeScope("/settings/onboarding/", "/settings/onboarding/{scope}", map[string]string{"GET": readScope, "POST": maintainScope, "PUT": maintainScope}),
	routeScope("/groups", "/groups", map[string]string{"GET": readScope, "POST": operateScope}),
	routeScope("/groups/", "/groups/{id}", map[string]string{"GET": readScope, "DELETE": operateScope}),
	routeScope("/groups/", "/groups/{id}/members", map[string]string{"GET": readScope, "POST": operateScope}),
	routeScope("/groups/", "/groups/{id}/members/{member}", map[string]string{"PATCH": operateScope, "DELETE": operateScope}),
	routeScope("/groups/", "/groups/{id}/leave", map[string]string{"POST": operateScope}),
	routeScope("/groups/", "/groups/{id}/messages", map[string]string{"GET": readScope, "POST": operateScope}),
	routeScope("/groups/", "/groups/{id}/read", map[string]string{"POST": operateScope}),
	routeScope("/mentions", "/mentions", map[string]string{"GET": readScope}),
	routeScope("/logs/daemon", "/logs/daemon", map[string]string{"GET": maintainScope}),
	routeScope("/fs/validate", "/fs/validate", map[string]string{"POST": maintainScope}),
	routeScope("/fs/detect", "/fs/detect", map[string]string{"GET": maintainScope}),
	routeScope("/ai/chat", "/ai/chat", map[string]string{"POST": operateScope}),
	routeScope("/ai/chat/stream", "/ai/chat/stream", map[string]string{"POST": operateScope}),
	routeScope("/ai/embeddings", "/ai/embeddings", map[string]string{"POST": operateScope}),
	routeScope("/ai/providers", "/ai/providers", map[string]string{"GET": readScope}),
	routeScope("/ai/models", "/ai/models", map[string]string{"GET": readScope}),
	routeScope("/ai/routes", "/ai/routes", map[string]string{"GET": readScope}),
	routeScope("/ai/routes/explain", "/ai/routes/explain", map[string]string{"POST": readScope}),
	routeScope("/ai/routes/preview", "/ai/routes/preview", map[string]string{"POST": readScope}),
	routeScope("/ai/usage", "/ai/usage", map[string]string{"GET": maintainScope}),
	routeScope("/ai/budgets", "/ai/budgets", map[string]string{"GET": maintainScope}),
	routeScope("/ai/audit", "/ai/audit", map[string]string{"GET": maintainScope}),
	routeScope("/a2a/", "/a2a/", map[string]string{"GET": readScope}),
	routeScope("/a2a/", "/a2a/agents/{binding}/.well-known/agent-card.json", map[string]string{"GET": readScope}),
	routeScope("/a2a/", "/a2a/agents/{binding}/tasks/{task}/transition", map[string]string{"POST": operateScope}),
	{Registration: "/a2a/", Path: "/a2a/agents/{binding}/rpc", Methods: map[string]string{"POST": selfScope}, PayloadPolicy: "a2a-rpc", PayloadScopes: map[string]string{
		"GetTask": readScope, "ListTasks": readScope, "SubscribeToTask": readScope,
		"GetTaskPushNotificationConfig": readScope, "ListTaskPushNotificationConfigs": readScope, "GetExtendedAgentCard": readScope,
		"SendMessage": operateScope, "SendStreamingMessage": operateScope, "CancelTask": operateScope,
		"CreateTaskPushNotificationConfig": maintainScope, "DeleteTaskPushNotificationConfig": maintainScope,
	}},
	routeScope("/mcp", "/mcp", map[string]string{"GET": localScope, "POST": localScope, "DELETE": localScope}),
	routeScope("/p/", "/p/{rest...}", map[string]string{"GET": localScope, "POST": localScope, "DELETE": localScope}),
	routeScope("/teams/roster", "/teams/roster", map[string]string{"GET": readScope}),
	routeScope("/teams/form", "/teams/form", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/dissolve", "/teams/dissolve", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/member_add", "/teams/member_add", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/member_remove", "/teams/member_remove", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/assign", "/teams/assign", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/delegate", "/teams/delegate", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/address", "/teams/address", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/cancel", "/teams/cancel", map[string]string{"POST": operateScope}, "team-idempotency"),
	routeScope("/teams/report_result", "/teams/report_result", map[string]string{"POST": operateScope}, "team-idempotency"),
}

func matchScopePath(pattern, escapedPath string) (int, bool) {
	want := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	got := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	score := 0
	for i, part := range want {
		if part == "{rest...}" {
			return score, i < len(got)
		}
		if part == "" && i < len(got) && got[i] == "" {
			continue
		}
		if i >= len(got) || got[i] == "" {
			return 0, false
		}
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			continue
		}
		value, err := url.PathUnescape(got[i])
		if err != nil || value != part {
			return 0, false
		}
		score++
	}
	return score, len(want) == len(got)
}

func remoteScopeRule(r *http.Request) *RouteScope {
	best := -1
	var selected *RouteScope
	for i := range RemoteRouteScopes {
		rule := &RemoteRouteScopes[i]
		if score, match := matchScopePath(rule.Path, r.URL.EscapedPath()); match && score > best {
			best, selected = score, rule
		}
	}
	return selected
}

func RequiredRemoteScope(r *http.Request) (string, string, bool) {
	rule := remoteScopeRule(r)
	if rule == nil {
		return "", "", false
	}
	scope, ok := rule.Methods[r.Method]
	return scope, rule.PayloadPolicy, ok
}

func RemoteScopeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !identity.IsRemote(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		scope, payload, known := RequiredRemoteScope(r)
		if !known {
			writeError(w, 404, CodeNotFound, "remote route or method unavailable")
			return
		}
		if scope == publicScope {
			next.ServeHTTP(w, r)
			return
		}
		if scope == localScope {
			if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/p/") {
				writeError(w, 404, CodeNotFound, "remote route unavailable")
			} else {
				writeError(w, 403, CodeForbidden, "route requires local custody")
			}
			return
		}
		p, ok := identity.FromContext(r.Context())
		if !ok || p.Kind != "device" || p.ID == identity.OperatorID {
			writeError(w, 401, "unauthorized", "verified device required")
			return
		}
		if payload == "a2a-rpc" {
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				writeError(w, 400, CodeInvalidRequest, "invalid RPC payload")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			var rpc struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(data, &rpc) != nil {
				writeError(w, 400, CodeInvalidRequest, "invalid RPC payload")
				return
			}
			var known bool
			scope, known = remoteScopeRule(r).PayloadScopes[rpc.Method]
			if !known {
				writeError(w, 403, CodeForbidden, "remote RPC method unavailable")
				return
			}
		}
		if scope != selfScope && !identity.HasDeviceScope(p, scope) {
			writeError(w, 403, "scope_required", "required scope: "+scope)
			return
		}
		if payload == "team-idempotency" {
			key := r.Header.Get("Idempotency-Key")
			if key != "" {
				if msg := validateIdempotencyKey(key); msg != "" {
					writeError(w, 400, CodeInvalidRequest, msg)
					return
				}
				key, err := principalIdempotencyKey(r.Context(), key)
				if err != nil {
					writeError(w, 401, "unauthorized", "verified device required")
					return
				}
				r = r.Clone(r.Context())
				r.Header.Set("Idempotency-Key", key)
			}
		}
		if payload == "registry-kind" {
			parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/registry/"), "/")
			kind, _ := url.PathUnescape(parts[0])
			if _, known := kindFromSegment(kind); !known {
				writeError(w, 404, CodeNotFound, "remote registry kind unavailable")
				return
			}
		}
		if payload == "launch" && r.Method == http.MethodPost {
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				writeError(w, 400, CodeInvalidRequest, "invalid launch payload")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			var req LaunchRequest
			if err := json.Unmarshal(data, &req); err != nil {
				writeError(w, 400, CodeInvalidRequest, "invalid launch payload")
				return
			}
			if (req.AgentFile != "" || req.AgentInline != "" || req.Override != "" || req.BootProfileFile != "" || req.Injection != "") && !identity.HasDeviceScope(p, maintainScope) {
				writeError(w, 403, "scope_required", "custom launch policy requires maintain scope")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
