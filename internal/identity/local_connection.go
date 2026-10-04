package identity

import (
	"context"
	"net"
)

type localConnectionKey struct{}

// ConnectionContext records the accepted socket type, never a request field.
// A local connection alone is not identity: callers still need a verified
// operator credential. Unix peer identity can be added at this boundary later.
func ConnectionContext(ctx context.Context, conn net.Conn) context.Context {
	_, local := conn.(*net.UnixConn)
	return context.WithValue(ctx, localConnectionKey{}, local)
}
func LocalConnection(ctx context.Context) bool {
	local, _ := ctx.Value(localConnectionKey{}).(bool)
	return local
}

// WithConnectionContext preserves accepted-socket proof across an internal
// dispatch boundary. The source must be the server's connection context;
// request metadata and caller-selected listener names cannot establish proof.
func WithConnectionContext(ctx, source context.Context) context.Context {
	local := source != nil && LocalConnection(source)
	return context.WithValue(ctx, localConnectionKey{}, local)
}
