package mcptransport

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	ErrRecoveryReadForbidden   = errors.New("recovery read forbidden")
	ErrRecoveryReadUnavailable = errors.New("recovery read unavailable")
)

// ReadRecoveryTool is a private daemon composition port. sourceSessionID is
// canonical retained identity, not a claimed transport principal. Its sealed
// policy and still-valid authority are supplied by the daemon-owned resolver.
// Current catalog restrictions intersect that floor. This path never creates
// a runtime, activates an origin, issues authority, or refreshes inventory.
func (h *Handler) ReadRecoveryTool(ctx context.Context, sourceSessionID, kind, tool string, args map[string]any) (json.RawMessage, error) {
	allowed := kind == "torque" && (tool == "torque_task_get" || tool == "torque_task_list") || kind == "tesseract" && tool == "tesseract_recall"
	if !allowed || sourceSessionID == "" || h.cfg.Resolver.RecoverySession == nil {
		return nil, ErrRecoveryReadForbidden
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrRecoveryReadUnavailable
	}
	h.calls.Add(1)
	h.mu.Unlock()
	defer h.calls.Done()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	stop := context.AfterFunc(h.ctx, cancel)
	defer func() { stop(); cancel() }()
	if h.cfg.Resolver.Catalog == nil {
		return nil, ErrRecoveryReadUnavailable
	}
	cat, err := h.cfg.Resolver.Catalog(ctx)
	if err != nil || cat == nil {
		return nil, ErrRecoveryReadUnavailable
	}
	policy, err := h.cfg.Resolver.RecoverySession(ctx, sourceSessionID)
	if err != nil || policy.SessionID != sourceSessionID || policy.Validate() != nil {
		return nil, ErrRecoveryReadForbidden
	}
	if cat.ValidateMCPGrant("retained recovery", policy.Servers) != nil {
		return nil, ErrRecoveryReadForbidden
	}
	opts, err := ViewOptions(Caller{Policy: policy}, cat.Global.MCP, nil, nil)
	if err != nil {
		return nil, ErrRecoveryReadForbidden
	}
	selection, err := mcpgateway.ResolveMode(opts.ModeInputs)
	if err != nil {
		return nil, ErrRecoveryReadForbidden
	}
	grant := append([]string{}, policy.Servers...)
	known := make(map[string]bool, len(cat.MCPServerEnabled)+1)
	for id, enabled := range cat.MCPServerEnabled {
		known[id] = enabled
	}
	known["tether"] = true
	for _, floor := range opts.AuthorityProfiles {
		grant, err = mcpgateway.SelectOrigins(known, grant, floor.Profile)
		if err != nil {
			return nil, ErrRecoveryReadForbidden
		}
	}
	grant, err = mcpgateway.SelectOrigins(known, grant, opts.Profile.Profile)
	if err != nil {
		return nil, ErrRecoveryReadForbidden
	}
	// Do not wait behind an upstream initialization with an unbounded mutex.
	if !h.runtimeMu.TryLock() {
		return nil, ErrRecoveryReadUnavailable
	}
	pool := h.runtime
	h.runtimeMu.Unlock()
	if pool == nil {
		return nil, ErrRecoveryReadUnavailable
	}
	view, err := pool.OpenView("retained recovery", grant, selection)
	if err != nil {
		return nil, ErrRecoveryReadUnavailable
	}
	view.Service.Policy = &mcpgateway.Policy{Selection: opts.Profile, Floors: opts.AuthorityProfiles}
	entry, err := view.Service.ResolveTarget(tool)
	if err != nil {
		return nil, ErrRecoveryReadForbidden
	}
	snapshot := view.Service.Snapshot()
	for _, collision := range snapshot.Collisions {
		if collision.Name == tool {
			return nil, ErrRecoveryReadForbidden
		}
	}
	for _, origin := range snapshot.Origins {
		if origin.ID == entry.Origin && origin.Error != "" {
			return nil, ErrRecoveryReadUnavailable
		}
	}
	// The resolved tool may be served by a shared engine origin. Its actual
	// unambiguous registry owner must be in the intersected grant. Annotation
	// validation is additional to authority, never its replacement.
	if entry.Tool.Annotations == nil || !entry.Tool.Annotations.ReadOnlyHint {
		return nil, ErrRecoveryReadForbidden
	}
	result, err := view.Service.Call(ctx, tool, args, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrRecoveryReadUnavailable
	}
	if result == nil || result.IsError {
		return nil, ErrRecoveryReadUnavailable
	}
	var raw []byte
	if result.StructuredContent != nil {
		raw, err = json.Marshal(result.StructuredContent)
	} else if len(result.Content) == 1 {
		if content, ok := result.Content[0].(*mcpsdk.TextContent); ok {
			raw = []byte(content.Text)
		}
	}
	if err != nil || len(raw) == 0 || len(raw) > 24*1024 || !json.Valid(raw) {
		return nil, ErrRecoveryReadUnavailable
	}
	return raw, nil
}
