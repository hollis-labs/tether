package mcpadapter

import (
	"fmt"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisteredTool associates a tool definition with its upstream source.
// Client is nil for native tether tools.
type RegisteredTool struct {
	Definition   *mcpsdk.Tool
	ServerID     string // upstream server ID; empty string = native tool
	Client       upstreamClient
	UpstreamName string // exact name sent to the upstream after final-name lookup
	conformance  *definitionLint
}

// definitionLint memoizes an accepted immutable declaration. The once callback
// always runs after registry/pool locks have been released.
type definitionLint struct {
	once     sync.Once
	ready    atomic.Bool
	findings []mcpgateway.NameFinding
}

// ToolRegistry holds the merged tool set: native tether tools plus all proxied
// upstream tools. It is safe for concurrent reads and writes.
type ToolRegistry struct {
	tools              map[string]RegisteredTool
	mu                 sync.RWMutex
	prefixes           map[string]string
	collisions         map[string][]mcpgateway.NameCollision
	reserved           map[string]mcpgateway.ToolOwner
	instructionLengths map[string]int
	lintTool           func(string, *mcpsdk.Tool) []mcpgateway.NameFinding
}

// ToolDelta describes the net effect of replacing one upstream server's tool set.
type ToolDelta struct {
	Added   []*mcpsdk.Tool
	Updated []*mcpsdk.Tool
	Removed []string
}

// NewToolRegistry creates an empty registry.
func NewToolRegistry() *ToolRegistry {
	r := &ToolRegistry{lintTool: mcpgateway.LintTool, instructionLengths: map[string]int{}, tools: make(map[string]RegisteredTool), prefixes: map[string]string{}, collisions: map[string][]mcpgateway.NameCollision{}, reserved: map[string]mcpgateway.ToolOwner{}}
	for _, name := range []string{"tether_gateway_status", "tether_tool_search", "tether_tool_list", "tether_tool_call"} {
		r.reserved[name] = mcpgateway.ToolOwner{Origin: "tether", Name: name, Kind: "gateway"}
	}
	return r
}

// SetPrefix configures an exact catalog prefix before this origin registers.
func (r *ToolRegistry) SetPrefix(serverID, prefix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefixes[serverID] = prefix
}
func (r *ToolRegistry) RegisterNative(tools []*mcpsdk.Tool) error { return r.Register("", nil, tools) }

// Register rejects the entire batch if any final name collides. No arrival-order
// aliases are created; successful registration preserves upstream definitions.
func (r *ToolRegistry) Register(serverID string, client upstreamClient, tools []*mcpsdk.Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	candidates, err := r.checkLocked(serverID, client, tools, false)
	if err != nil {
		return err
	}
	for key, rt := range candidates {
		r.tools[key] = rt
	}
	delete(r.collisions, serverID)
	r.pruneCollisionsLocked()
	return nil
}
func owner(rt RegisteredTool) mcpgateway.ToolOwner {
	origin, kind := rt.ServerID, "upstream"
	if origin == "" {
		origin, kind = "tether", "native"
	}
	return mcpgateway.ToolOwner{Origin: origin, Name: rt.UpstreamName, Kind: kind}
}
func (r *ToolRegistry) checkLocked(serverID string, client upstreamClient, tools []*mcpsdk.Tool, replace bool) (map[string]RegisteredTool, error) {
	candidates := map[string]RegisteredTool{}
	collisions := []mcpgateway.NameCollision{}
	for _, t := range tools {
		if t == nil {
			return nil, fmt.Errorf("origin %q returned a nil tool definition", serverID)
		}
		name, def := r.prefixes[serverID]+t.Name, t
		if name != t.Name {
			copyTool := *t
			copyTool.Name = name
			def = &copyTool
		}
		incoming := RegisteredTool{Definition: def, ServerID: serverID, Client: client, UpstreamName: t.Name, conformance: &definitionLint{}}
		if previous, ok := r.tools[name]; ok && previous.Definition == def && previous.ServerID == serverID {
			incoming.conformance = previous.conformance
		}
		other, exists := r.reserved[name]
		if !exists {
			if rt, ok := candidates[name]; ok {
				other, exists = owner(rt), true
			} else if rt, ok := r.tools[name]; ok && (!replace || rt.ServerID != serverID) {
				other, exists = owner(rt), true
			}
		}
		if exists {
			collisions = append(collisions, mcpgateway.CollidingName(name, other, owner(incoming)))
		}
		candidates[name] = incoming
	}
	if len(collisions) > 0 {
		sort.Slice(collisions, func(i, j int) bool { return collisions[i].Name < collisions[j].Name })
		r.collisions[serverID] = collisions
		return nil, &mcpgateway.CollisionError{Collisions: collisions}
	}
	return candidates, nil
}

// Lookup returns the RegisteredTool for the given tool name (thread-safe).
func (r *ToolRegistry) Lookup(name string) (RegisteredTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.tools[name]
	return rt, ok
}

// AllDefinitions returns all tool definitions sorted by name, suitable for
// returning in a tools/list response.
func (r *ToolRegistry) AllDefinitions() []*mcpsdk.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]*mcpsdk.Tool, 0, len(r.tools))
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
	delete(r.collisions, serverID)
	for key, rt := range r.tools {
		if rt.ServerID == serverID {
			delete(r.tools, key)
		}
	}
	r.pruneCollisionsLocked()
}

// ReplaceServer atomically swaps one upstream server's registered tool set and
// returns the added, updated, and removed tool names/definitions.
func (r *ToolRegistry) ReplaceServer(serverID string, client upstreamClient, tools []*mcpsdk.Tool) (ToolDelta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	candidates, err := r.checkLocked(serverID, client, tools, true)
	if err != nil {
		return ToolDelta{}, err
	}
	old := map[string]RegisteredTool{}
	for key, rt := range r.tools {
		if rt.ServerID == serverID {
			old[key] = rt
			delete(r.tools, key)
		}
	}
	newDefs := make(map[string]*mcpsdk.Tool, len(candidates))
	for key, rt := range candidates {
		r.tools[key] = rt
		newDefs[key] = rt.Definition
	}
	delete(r.collisions, serverID)
	r.pruneCollisionsLocked()

	delta := ToolDelta{
		Added:   make([]*mcpsdk.Tool, 0),
		Updated: make([]*mcpsdk.Tool, 0),
		Removed: make([]string, 0),
	}
	for key, def := range newDefs {
		oldRT, existed := old[key]
		switch {
		case !existed:
			delta.Added = append(delta.Added, def)
		case !reflect.DeepEqual(oldRT.Definition, def):
			delta.Updated = append(delta.Updated, def)
		}
	}
	for key := range old {
		if _, ok := newDefs[key]; !ok {
			delta.Removed = append(delta.Removed, key)
		}
	}

	sort.Slice(delta.Added, func(i, j int) bool { return delta.Added[i].Name < delta.Added[j].Name })
	sort.Slice(delta.Updated, func(i, j int) bool { return delta.Updated[i].Name < delta.Updated[j].Name })
	sort.Strings(delta.Removed)
	return delta, nil
}

// RegisterLocal stores native definitions and a local protocol dispatch seam.
// They keep an empty ServerID so upstream confinement never selects them.
func (r *ToolRegistry) RegisterLocal(tool *mcpsdk.Tool, client upstreamClient) error {
	return r.Register("", client, []*mcpsdk.Tool{tool})
}

// RecordInstructions retains a length only, scoped to the observed origin.
func (r *ToolRegistry) RecordInstructions(origin string, length int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instructionLengths[origin] = length
}

// Collisions is the cheap path for callers that do not consume lint. It never
// invokes a linter, including while a pool caller holds its status mutex.
func (r *ToolRegistry) Collisions() []mcpgateway.NameCollision {
	r.mu.RLock()
	collisions := r.collisionsLocked()
	r.mu.RUnlock()
	sortCollisions(collisions)
	return collisions
}
func (r *ToolRegistry) collisionsLocked() []mcpgateway.NameCollision {
	out := []mcpgateway.NameCollision{}
	for _, items := range r.collisions {
		out = append(out, items...)
	}
	return out
}
func sortCollisions(collisions []mcpgateway.NameCollision) {
	sort.Slice(collisions, func(i, j int) bool {
		return collisions[i].Name+collisions[i].Owners[0].Origin < collisions[j].Name+collisions[j].Owners[0].Origin
	})
}

// At most 128 new declarations and 512 KiB of conservatively estimated schema
// work are examined per diagnostic pass. Cached
// findings do not consume the budget; later passes can examine the remainder.
const conformancePassTools = 128
const conformancePassBytes = 512 * 1024

func (r *ToolRegistry) NameDiagnostics() ([]mcpgateway.NameFinding, []mcpgateway.NameCollision) {
	r.mu.RLock()
	definitions := make([]RegisteredTool, 0, len(r.tools))
	for _, rt := range r.tools {
		definitions = append(definitions, rt)
	}
	lengths := make(map[string]int, len(r.instructionLengths))
	for origin, length := range r.instructionLengths {
		lengths[origin] = length
	}
	collisions := r.collisionsLocked()
	lint := r.lintTool
	r.mu.RUnlock()
	// Deterministic budget order and no linter work under registry or pool locks.
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Definition.Name < definitions[j].Definition.Name })
	findings := []mcpgateway.NameFinding{}
	examined, bytes := 0, 0
	exhausted := false
	for _, rt := range definitions {
		cache := rt.conformance
		if cache != nil && !cache.ready.Load() && !exhausted {
			cost := mcpgateway.SchemaLintCost(rt.Definition.InputSchema)
			if examined >= conformancePassTools || cost > conformancePassBytes-bytes {
				exhausted = true
			} else {
				examined++
				bytes += cost
			}
		}
		if cache == nil || !cache.ready.Load() && exhausted {
			findings = append(findings, mcpgateway.NameFinding{Origin: owner(rt).Origin, Name: rt.Definition.Name, Code: "conformance_unexamined", Message: "tool declaration unexamined: aggregate conformance budget exhausted; a later status pass can examine it"})
			continue
		}
		cache.once.Do(func() { cache.findings = lint(owner(rt).Origin, rt.Definition); cache.ready.Store(true) })
		findings = append(findings, cache.findings...)
	}
	for origin, length := range lengths {
		findings = append(findings, mcpgateway.LintInstructions(origin, length)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		return a.Origin+"\x00"+a.Name+"\x00"+a.Code < b.Origin+"\x00"+b.Name+"\x00"+b.Code
	})
	sortCollisions(collisions)
	return findings, collisions
}

// pruneCollisionsLocked removes findings whose accepted opposing owner no
// longer exposes the colliding name. Rejected definitions are never published.
func (r *ToolRegistry) pruneCollisionsLocked() {
	for origin, collisions := range r.collisions {
		active := collisions[:0]
		for _, collision := range collisions {
			_, reserved := r.reserved[collision.Name]
			rt, exists := r.tools[collision.Name]
			if reserved || collision.Owners[0].Origin == collision.Owners[1].Origin || (exists && owner(rt).Origin != origin) {
				active = append(active, collision)
			}
		}
		if len(active) == 0 {
			delete(r.collisions, origin)
		} else {
			r.collisions[origin] = active
		}
	}
}

// RebindServer keeps accepted schemas across reconnect when a new tools/list
// batch is rejected. Lookup still uses the final name and its accepted upstream
// name; newly announced conflicting names never enter dispatch.
func (r *ToolRegistry) RebindServer(id string, client upstreamClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, rt := range r.tools {
		if rt.ServerID == id {
			rt.Client = client
			r.tools[name] = rt
		}
	}
}
