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

// writeToolVerbs veto a read classification when they appear anywhere in the
// name, so compound names such as get_and_apply or find_and_delete are writes.
var writeToolVerbs = []string{"exec", "run", "delete", "remove", "apply", "fix", "install", "enroll", "restart", "write", "set", "create", "update", "and", "then"}

// IsReadOnlyMachineTool reports whether tool, called on server, only reads, going by its name alone.
func IsReadOnlyMachineTool(server, tool string) bool {
	return IsReadOnlyMachineToolWith(server, tool, nil)
}

// IsReadOnlyMachineToolWith classifies a machine tool, also using the upstream's
// declared annotations when the tool is registered. Order, strictest first:
//
//  1. destructiveHint=true is a write.
//  2. A write verb in any name segment is a write (get_and_apply, find_and_delete).
//  3. readOnlyHint=true is a read. The server is a catalog-defined machine
//     upstream (only those reach this check), so its declaration is trusted.
//  4. A read verb as the first or the last name segment is a read, so both
//     list_resources and resource_list are reads.
//  5. Everything else is a write (fail-closed).
func IsReadOnlyMachineToolWith(server, tool string, ann *mcpsdk.ToolAnnotations) bool {
	name := strings.ToLower(tool)
	name = strings.TrimPrefix(name, strings.ToLower(server)+"_")
	name = strings.TrimPrefix(name, strings.ToLower(server)+".")
	segs := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	if len(segs) == 0 {
		return false
	}
	if ann != nil && ann.DestructiveHint != nil && *ann.DestructiveHint {
		return false
	}
	for _, seg := range segs {
		if slices.Contains(writeToolVerbs, seg) {
			return false
		}
	}
	if ann != nil && ann.ReadOnlyHint {
		return true
	}
	return slices.Contains(readToolVerbs, segs[0]) || slices.Contains(readToolVerbs, segs[len(segs)-1])
}

// MachineAuthorityMiddleware enforces the agent's machine access on every call
// routed to a launch.MachineChangingMCPServers upstream and records each
// decision as a durable event. Calls to other servers pass through untouched.
type MachineAuthorityMiddleware struct {
	pub events.Publisher
	// registry, when set, supplies the upstream's declared annotations for a call.
	registry *ToolRegistry
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
	var ann *mcpsdk.ToolAnnotations
	if m.registry != nil {
		if rt, ok := m.registry.Lookup(call.ToolName); ok && rt.Definition != nil {
			ann = rt.Definition.Annotations
		}
	}
	if IsReadOnlyMachineToolWith(server, call.ToolName, ann) {
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
