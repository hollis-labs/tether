package identity

import "context"

type internalServiceKey struct{}

// WithInternalService marks ctx as a daemon-internal publisher speaking as the
// named service address (for example the turn router). It lives only in the
// process context: no HTTP request, header or token can set it, and it never
// sets Principal.Verified. A policy that wants to trust daemon-internal routing
// lists the service address explicitly rather than reading scopes.
func WithInternalService(ctx context.Context, serviceURN string) context.Context {
	return context.WithValue(ctx, internalServiceKey{}, serviceURN)
}

// InternalService returns the service address set by WithInternalService.
func InternalService(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(internalServiceKey{}).(string)
	return s, ok && s != ""
}
