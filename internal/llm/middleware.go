package llm

import "context"

// Handler is the terminal AI gateway execution function in a middleware chain.
type Handler func(ctx context.Context, req Request) (Response, error)

// Middleware wraps a Handler for cross-cutting concerns such as audit,
// budget enforcement, redaction, and route policy.
type Middleware interface {
	Handle(ctx context.Context, req Request, next Handler) (Response, error)
}

// BuildMiddlewareChain composes middleware around a terminal handler. The
// first middleware in the slice is the outermost wrapper.
func BuildMiddlewareChain(handler Handler, mws []Middleware) Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		inner := handler
		mwCopy := mw
		innerCopy := inner
		handler = func(ctx context.Context, req Request) (Response, error) {
			return mwCopy.Handle(ctx, req, innerCopy)
		}
	}
	return handler
}
