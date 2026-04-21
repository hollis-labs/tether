// Package mcpadapter exposes the agent-mux runtime as an MCP stdio server.
//
// Start the server with:
//
//	mux mcp [--token <tok>] [--scopes session.write,message.write]
//
// The adapter wraps app.Service directly (in-process) rather than going
// through the daemon HTTP API. All mutating tools require a token and the
// appropriate scope string.
//
// Scopes:
//
//	session.write  — create, launch, stop, wait, input, resize, resume
//	message.write  — send, consume, cancel
package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chrispian/agent-mux/internal/app"
)

const version = "0.1.0"

// Scope constants for mutating tool groups.
const (
	ScopeSessionWrite = "session.write"
	ScopeMessageWrite = "message.write"
)

// Adapter exposes the agent-mux runtime as MCP tools over stdio.
type Adapter struct {
	svc    *app.Service
	token  string
	scopes map[string]struct{}
}

// New constructs an Adapter wrapping svc. token and scopes gate mutating
// tools; pass an empty token to disable auth (development only).
func New(svc *app.Service, token string, scopes []string) *Adapter {
	scopeSet := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s != "" {
			scopeSet[s] = struct{}{}
		}
	}
	return &Adapter{
		svc:    svc,
		token:  strings.TrimSpace(token),
		scopes: scopeSet,
	}
}

// Run starts the MCP stdio server. It blocks until ctx is canceled or
// the stdio transport closes.
func (a *Adapter) Run(ctx context.Context) error {
	s := server.NewMCPServer(
		"agent-mux",
		version,
		server.WithToolCapabilities(true),
	)
	a.registerTools(s)
	ctxFunc := func(_ context.Context) context.Context { return ctx }
	return server.ServeStdio(s, server.WithStdioContextFunc(ctxFunc))
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// toolJSON serializes v as a JSON text tool result.
func toolJSON(v any) *mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return mcp.NewToolResultText(string(b))
}

// toolError returns a JSON error tool result with a code and message.
func toolError(code, message string) *mcp.CallToolResult {
	b, _ := json.Marshal(map[string]string{"ok": "false", "code": code, "message": message})
	return mcp.NewToolResultText(string(b))
}

// checkScope verifies that the token is set and the named scope is present.
// Returns a non-nil error result when the check fails.
func (a *Adapter) checkScope(scope string) *mcp.CallToolResult { //nolint:unparam // scope will expand beyond ScopeSessionWrite as more tool groups are registered
	if a.token == "" {
		return toolError("auth_required", "no token configured; pass --token to enable mutating tools")
	}
	if _, ok := a.scopes[scope]; !ok {
		return toolError("insufficient_scope", "token missing required scope: "+scope)
	}
	return nil
}

// str extracts a string argument from a CallToolRequest, returning "" if
// the key is absent or not a string.
func str(req mcp.CallToolRequest, key string) string {
	v, _ := req.GetArguments()[key].(string)
	return strings.TrimSpace(v)
}

// intArg extracts an integer argument. JSON numbers arrive as float64.
func intArg(req mcp.CallToolRequest, key string, def int) int {
	switch v := req.GetArguments()[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return def
	}
}
