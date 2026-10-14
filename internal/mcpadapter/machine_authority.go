package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// readToolVerbs are the leading name segments of a read-only machine tool.
// Classification is fail-closed: a tool whose name does not start with one of
// these (after the server prefix) is a write.
var readToolVerbs = []string{"list", "get", "show", "describe", "status", "read", "search", "health", "whoami", "inspect", "info", "check", "history", "audit", "ls", "find", "query"}

// IsReadOnlyMachineTool reports whether tool, called on server, only reads.
func IsReadOnlyMachineTool(server, tool string) bool {
	name := strings.ToLower(tool)
	name = strings.TrimPrefix(name, strings.ToLower(server)+"_")
	name = strings.TrimPrefix(name, strings.ToLower(server)+".")
	verb, _, _ := strings.Cut(name, "_")
	return slices.Contains(readToolVerbs, verb)
}

// MachineAuthorityMiddleware enforces the agent's machine access on every call
// routed to a launch.MachineChangingMCPServers upstream and records each
// decision as a durable event. Calls to other servers pass through untouched.
type MachineAuthorityMiddleware struct {
	pub events.Publisher
	// Access is launch.MachineAccessReadOnly or ReadWrite; anything else is
	// treated as read-only. Dev grants full access.
	Access string
	Dev    bool
}

// NewMachineAuthorityMiddleware builds the middleware from the proxy env
// lookup (os.Getenv in production). pub may be nil.
func NewMachineAuthorityMiddleware(pub events.Publisher, getenv func(string) string) *MachineAuthorityMiddleware {
	return &MachineAuthorityMiddleware{pub: pub, Access: getenv(launch.MachineAccessEnv), Dev: getenv(launch.MachineDevModeEnv) == "1"}
}

func (m *MachineAuthorityMiddleware) Handle(ctx context.Context, call ToolCall, next ToolCallHandler) (*mcpsdk.CallToolResult, error) {
	server := serverIDFromContext(ctx)
	if !slices.Contains(launch.MachineChangingMCPServers, server) {
		return next(ctx, call)
	}
	class := "write"
	if IsReadOnlyMachineTool(server, call.ToolName) {
		class = "read"
	}
	mode := launch.MachineAccessReadOnly
	switch {
	case m.Dev:
		mode = "dev"
	case m.Access == launch.MachineAccessReadWrite:
		mode = launch.MachineAccessReadWrite
	}
	allowed := class == "read" || mode == "dev" || mode == launch.MachineAccessReadWrite
	m.audit(ctx, events.MachineAuthorityDecision{Server: server, Tool: call.ToolName, Class: class, Mode: mode, Allowed: allowed, SessionID: sessionIDFromContext(ctx), Timestamp: time.Now().UTC()})
	if !allowed {
		telemetry.SetErrorClass(ctx, events.ToolErrorDenied)
		return errorResult(fmt.Sprintf("machine authority: %q is a write tool on %q and this agent has read-only machine access", call.ToolName, server)), nil
	}
	return next(ctx, call)
}

func (m *MachineAuthorityMiddleware) audit(ctx context.Context, d events.MachineAuthorityDecision) {
	slog.Info("machine authority decision", "server", d.Server, "tool", d.Tool, "class", d.Class, "mode", d.Mode, "allowed", d.Allowed)
	if m.pub == nil {
		return
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	scope := events.ScopeSession
	if d.SessionID == "" {
		scope = events.ScopeDaemon
	}
	if err := m.pub.Publish(context.WithoutCancel(ctx), events.Event{Scope: scope, SessionID: d.SessionID, Kind: events.EventTypeMachineAuthorityDecision, PayloadJSON: string(raw)}); err != nil {
		slog.Warn("machine authority: publish audit", "err", err)
	}
}
