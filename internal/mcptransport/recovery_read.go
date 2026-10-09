package mcptransport

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

var (
	ErrRecoveryReadForbidden   = errors.New("recovery read forbidden")
	ErrRecoveryReadUnavailable = errors.New("recovery read unavailable")
)

// ReadRecoveryTool is trusted host composition, with the target's verified
// principal in ctx. It uses the existing pool and the same grant/profile floors
// as normal dispatch. Recovery never constructs or activates an upstream.
func (h *Handler) ReadRecoveryTool(ctx context.Context, origin, tool string, args map[string]any) (json.RawMessage, error) {
	allowed := origin == "torque" && (tool == "torque_task_get" || tool == "torque_task_list") || origin == "tesseract" && tool == "tesseract_recall"
	if !allowed {
		return nil, ErrRecoveryReadForbidden
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if principal, ok := identity.FromContext(ctx); !ok || principal.ExpiresAt != nil && !principal.ExpiresAt.After(time.Now()) {
		return nil, ErrRecoveryReadForbidden
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrRecoveryReadUnavailable
	}
	h.calls.Add(1)
	h.mu.Unlock()
	defer h.calls.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	defer func() { stop(); cancel() }()
	cat, err := h.cfg.Resolver.Catalog(ctx)
	if err != nil || cat == nil {
		return nil, ErrRecoveryReadUnavailable
	}
	resolver := h.cfg.Resolver
	resolver.Catalog = func(context.Context) (*config.Catalog, error) { return cat, nil }
	caller, err := resolver.Resolve(ctx)
	if err != nil {
		if errors.Is(err, errSessionUnavailable) {
			return nil, ErrRecoveryReadUnavailable
		}
		return nil, ErrRecoveryReadForbidden
	}
	if !slices.Contains(caller.Policy.Servers, origin) {
		return nil, ErrRecoveryReadForbidden
	}
	opts, err := ViewOptions(caller, cat.Global.MCP, nil, nil)
	if err != nil {
		return nil, ErrRecoveryReadForbidden
	}
	opts.Only = true
	opts.Publisher = h.cfg.Publisher
	adapter, err := mcpadapter.NewVerifiedAdapter(ctx, h.cfg.Service, nil)
	if err != nil {
		return nil, ErrRecoveryReadForbidden
	}
	h.runtimeMu.Lock()
	pool := h.runtime
	if pool == nil {
		h.runtimeMu.Unlock()
		return nil, ErrRecoveryReadUnavailable
	}
	view, err := pool.NewGatewayView(ctx, adapter, opts)
	h.runtimeMu.Unlock()
	if err != nil {
		return nil, ErrRecoveryReadUnavailable
	}
	defer view.Close()
	entry, err := view.Service.ResolveTarget(tool)
	if err != nil {
		var target *mcpgateway.TargetError
		if errors.As(err, &target) && target.Unavailable {
			return nil, ErrRecoveryReadUnavailable
		}
		return nil, ErrRecoveryReadForbidden
	}
	if entry.Origin != origin {
		return nil, ErrRecoveryReadForbidden
	}
	var sessions api.CallerSessionLookup
	var bindings api.CallerBindingLookup
	if h.cfg.Service != nil {
		if h.cfg.Service.Store != nil {
			sessions = h.cfg.Service.Store
		}
		if h.cfg.Service.Registry != nil {
			bindings = h.cfg.Service.Registry
		}
	}
	ctx = callcontext.WithClaimedSession(ctx, caller.Principal.SessionID)
	ctx = callcontext.WithSnapshot(ctx, api.ResolveCallerContext(ctx, sessions, bindings))
	result, err := view.Service.Call(ctx, tool, args, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrRecoveryReadUnavailable
	}
	if result == nil {
		return nil, ErrRecoveryReadUnavailable
	}
	return json.Marshal(result)
}
