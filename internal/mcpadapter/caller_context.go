package mcpadapter

import (
	"context"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/identity"
)

type callerContextCache struct {
	mu       sync.Mutex
	snapshot callcontext.Snapshot
	until    time.Time
}

// SetCallerContextResolver supplies a credentialed daemon lookup for stdio
// adapters. It never receives a claimed session selector.
func (a *Adapter) SetCallerContextResolver(fn func(context.Context) (callcontext.Snapshot, error)) {
	a.callerContextCache.mu.Lock()
	defer a.callerContextCache.mu.Unlock()
	a.callerContextResolver = fn
	a.callerContextCache.until = time.Time{}
}

func (a *Adapter) withCallerContext(ctx context.Context) context.Context {
	if _, exists := callcontext.FromContext(ctx); exists {
		return ctx
	}
	if _, verified := identity.FromContext(ctx); verified && a.svc != nil && a.svc.Store != nil {
		var bindings api.CallerBindingLookup
		if a.svc.Registry != nil {
			bindings = a.svc.Registry
		}
		return callcontext.WithSnapshot(ctx, api.ResolveCallerContext(ctx, a.svc.Store, bindings))
	}
	a.callerContextCache.mu.Lock()
	defer a.callerContextCache.mu.Unlock()
	if time.Now().Before(a.callerContextCache.until) {
		return callcontext.WithSnapshot(ctx, a.callerContextCache.snapshot)
	}
	resolve := a.callerContextResolver
	if resolve == nil && a.client != nil {
		resolve = a.client.CallerContext
	}
	var snapshot callcontext.Snapshot
	if resolve != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		resolved, err := resolve(lookupCtx)
		cancel()
		if err == nil {
			snapshot = resolved
		}
	}
	// Anonymous results and failures share a short negative cache. This is
	// attribution only: authorization and revocation are checked per request.
	ttl := 3 * time.Second
	if snapshot.PrincipalID != "" {
		ttl = 5 * time.Second
	}
	a.callerContextCache.snapshot = snapshot
	a.callerContextCache.until = time.Now().Add(ttl)
	return callcontext.WithSnapshot(ctx, snapshot)
}
