// Package mcpadapter exposes the agent-mux runtime as an MCP stdio server.
//
// Start the server with:
//
//	mux mcp [--token <tok>] [--scopes session.write,message.write]
//
// The adapter wraps app.Service for catalog reads, message ops, and
// read-only session inspection. Session-mutating tools (create, launch,
// stop, send, resize, wait, logical-agent resume) route through the
// running muxd daemon over UDS via internal/client.Client to avoid the
// split-brain that an in-process app.New() instance would produce
// against daemon-owned session state. v005-09 introduced the daemon
// routing — see ADR 0034 (or 0035 if split). All mutating tools require
// a token and the appropriate scope string.
//
// Scopes:
//
//	session.write  — create, launch, stop, wait, input, resize, resume
//	message.write  — send, consume, cancel
package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	mcpsanitize "github.com/hollis-labs/go-mcp-sanitize"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
)

const version = "0.2.0"

// Scope constants for mutating tool groups.
const (
	ScopeSessionWrite = "session.write"
	ScopeMessageWrite = "message.write"
)

// Adapter exposes the agent-mux runtime as MCP tools over stdio.
type Adapter struct {
	svc    *app.Service
	client *client.Client // optional; when set, session-mutating tools route through the daemon
	token  string
	scopes map[string]struct{}

	// Logger receives the warn-level telemetry emitted by the
	// go-mcp-sanitize middleware when it cleans a polluted tool call.
	// Optional; nil falls back to slog.Default(). The MCP stdio command
	// wires this to stderr so warn lines do not collide with the protocol
	// stream on stdout.
	Logger *slog.Logger
}

// New constructs an Adapter wrapping svc. token and scopes gate mutating
// tools; pass an empty token to disable auth (development only).
//
// In this mode session-mutating tools execute against svc directly
// in-process. Use NewWithDaemon for the production "mux mcp" path so
// session ownership stays with the daemon.
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

// NewWithDaemon constructs an Adapter that routes session-mutating tools
// through the running muxd daemon at dc, while keeping catalog reads,
// message ops, and read-only session inspection in-process via svc.
//
// Use this for the production "mux mcp" subcommand. dc must not be nil
// — pass New for in-process-only mode.
func NewWithDaemon(svc *app.Service, dc *client.Client, token string, scopes []string) *Adapter {
	a := New(svc, token, scopes)
	a.client = dc
	return a
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

// addTool wraps every MCP tool handler with the go-mcp-sanitize middleware,
// which auto-cleans malformed agent tool-call XML in free-text params before
// the handler runs. Clean calls are silent; cleaned calls emit one warn-level
// slog line via a.Logger (see github.com/hollis-labs/go-mcp-sanitize).
//
// All registerXxx helpers must call a.addTool(s, tool, handler) instead of
// s.AddTool(tool, handler) directly so the protection stays uniform across
// every tool surface registered by the adapter.
func (a *Adapter) addTool(s *server.MCPServer, t mcp.Tool, h server.ToolHandlerFunc) {
	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s.AddTool(t, mcpsanitize.Middleware(logger)(h))
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// toolJSON serializes v as a JSON text tool result.
func toolJSON(v any) *mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return mcp.NewToolResultText(string(b))
}

// toolError returns an MCP error result (IsError=true) with a structured JSON body.
// Using NewToolResultError ensures middleware and clients that check result.IsError
// correctly identify failures — NewToolResultText with ok:"false" was wrong.
func toolError(code, message string) *mcp.CallToolResult {
	b, _ := json.Marshal(map[string]any{"ok": false, "code": code, "message": message})
	return mcp.NewToolResultError(string(b))
}

// daemonUnreachableError is the canonical tool-error response when a
// session-mutating tool routes through the daemon but the daemon is not
// running or otherwise unreachable. Code "daemon_unavailable" matches
// ADR 0010's typed error envelope conventions; the message is actionable.
func daemonUnreachableError(err error) *mcp.CallToolResult {
	return toolError("daemon_unavailable",
		"muxd daemon is not reachable; start it with `mux daemon up` ("+err.Error()+")")
}

// isDaemonUnreachable reports whether err signals that the daemon is not
// running (socket missing, connection refused, dial timeout). Wraps
// client.ErrDaemonUnreachable so handlers can branch on transport-vs-domain
// failure consistently.
func isDaemonUnreachable(err error) bool {
	return errors.Is(err, client.ErrDaemonUnreachable)
}

// classifyClientErr maps a daemon HTTP error string into the same MCP
// error codes the in-process path produces (not_found / conflict /
// internal_error). The daemon already classifies via ADR 0010 typed
// envelopes; we string-sniff the wrapped form ("daemon NNN (code): msg")
// to recover the code without reaching into internal/api here.
func classifyClientErr(err error, id string) *mcp.CallToolResult {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "(not_found)"), strings.Contains(msg, " 404 "):
		return toolError("not_found", "session not found: "+id)
	case strings.Contains(msg, "(conflict)"), strings.Contains(msg, " 409 "):
		return toolError("conflict", err.Error())
	default:
		return toolError("internal_error", err.Error())
	}
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

// intArg extracts an integer argument. JSON numbers arrive as float64 from
// well-behaved callers, but LLM clients frequently emit numeric strings (e.g.
// "50"). Both forms are handled; unknown types fall back to def.
func intArg(req mcp.CallToolRequest, key string, def int) int {
	switch v := req.GetArguments()[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
		return def
	case string:
		if v == "" {
			return def
		}
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
		return def
	default:
		return def
	}
}
