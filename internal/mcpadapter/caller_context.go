package mcpadapter

import (
	"context"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/identity"
)

// SetCallerContextResolver supplies a credentialed daemon lookup for stdio
// adapters. It never receives a claimed session selector.
func (a *Adapter) SetCallerContextResolver(fn func(context.Context) (callcontext.Snapshot, error)) {
	a.callerContextResolver = fn
}

func (a *Adapter) withCallerContext(ctx context.Context) context.Context {
	if _, exists := callcontext.FromContext(ctx); exists {
		return ctx
	}
	if _, verified := identity.FromContext(ctx); verified && a.svc != nil && a.svc.Store != nil {
		return callcontext.WithSnapshot(ctx, api.ResolveCallerContext(ctx, a.svc.Store, a.svc.Registry))
	}
	resolve := a.callerContextResolver
	if resolve == nil && a.client != nil {
		resolve = a.client.CallerContext
	}
	var snapshot callcontext.Snapshot
	if resolve != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
		resolved, err := resolve(lookupCtx)
		cancel()
		if err == nil {
			snapshot = resolved
		}
	}
	return callcontext.WithSnapshot(ctx, snapshot)
}
