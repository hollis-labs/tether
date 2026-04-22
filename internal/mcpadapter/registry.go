package mcpadapter

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// registeredTool associates a tool definition with its upstream source.
// Client is nil for native mux tools.
type registeredTool struct {
	Definition mcp.Tool
	ServerID   string // upstream server ID; empty string = native tool
	Client     mcpclient.MCPClient
}

// ToolRegistry holds the merged tool set: native mux tools plus all proxied
// upstream tools. It is safe for concurrent reads and writes.
type ToolRegistry struct {
	tools map[string]registeredTool
	mu    sync.RWMutex
}

// NewToolRegistry creates an empty registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]registeredTool)}
}

// RegisterNative bulk-registers native mux tools (serverID = "", client = nil).
func (r *ToolRegistry) RegisterNative(tools []mcp.Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		r.tools[t.Name] = registeredTool{Definition: t}
	}
}

// Register bulk-registers tools from one upstream server. On name collision
// with an already-registered tool it logs a warning and stores the new entry
// under "<serverID>__<toolName>" to preserve both.
func (r *ToolRegistry) Register(serverID string, client mcpclient.MCPClient, tools []mcp.Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		key := t.Name
		if existing, exists := r.tools[key]; exists {
			slog.Warn("mcp-proxy: tool name collision",
				"tool", t.Name,
				"existing_server", existing.ServerID,
				"incoming_server", serverID,
				"disambiguated_as", fmt.Sprintf("%s__%s", serverID, t.Name),
			)
			// Disambiguate the incoming tool; keep the existing one at its original key.
			key = fmt.Sprintf("%s__%s", serverID, t.Name)
			disambig := t
			disambig.Name = key
			t = disambig
		}
		r.tools[key] = registeredTool{
			Definition: t,
			ServerID:   serverID,
			Client:     client,
		}
	}
}

// Lookup returns the registeredTool for the given tool name (thread-safe).
func (r *ToolRegistry) Lookup(name string) (registeredTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.tools[name]
	return rt, ok
}

// AllDefinitions returns all tool definitions sorted by name, suitable for
// returning in a tools/list response.
func (r *ToolRegistry) AllDefinitions() []mcp.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]mcp.Tool, 0, len(r.tools))
	for _, rt := range r.tools {
		defs = append(defs, rt.Definition)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// RemoveServer removes all tools registered for the given upstream server ID.
func (r *ToolRegistry) RemoveServer(serverID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, rt := range r.tools {
		if rt.ServerID == serverID {
			delete(r.tools, key)
		}
	}
}

// ToolCount returns the number of tools registered for serverID.
func (r *ToolRegistry) ToolCount(serverID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, rt := range r.tools {
		if rt.ServerID == serverID {
			count++
		}
	}
	return count
}
