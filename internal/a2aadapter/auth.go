package a2aadapter

// auth.go — a pluggable a2asrv.CallInterceptor requiring a bearer token,
// checked before any request reaches TetherExecutor. Auth is a protocol-
// level extension point the SDK provides (a2asrv.CallInterceptor), not a
// bespoke scheme layered on top of it (T10 acceptance #3: "auth tests use
// the canonical service" -- here, "canonical" means the SDK's own
// documented auth extension point, exercised as designed, rather than
// Tether inventing a parallel mechanism the protocol doesn't know about).

import (
	"context"
	"crypto/subtle"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// matchesBearerToken reports whether got (a raw `Authorization` header
// value) presents token as a bearer credential. Constant-time: token
// values are secrets, and a naive == comparison timing-leaks how many
// leading bytes matched. Shared by bearerTokenInterceptor (the JSON-RPC
// path, checked via the SDK's own CallInterceptor extension point) and
// checkTransitionBearerToken (transition.go's raw http.HandlerFunc,
// which the SDK's interceptor chain never touches at all -- see that
// file's doc comment for why it needs its own, equivalent check).
func matchesBearerToken(got, token string) bool {
	if token == "" {
		return true
	}
	want := "Bearer " + token
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// bearerTokenInterceptor requires `Authorization: Bearer <token>` to
// match Token exactly. An empty Token disables the check entirely --
// same-host-only fixture/test bindings are expected to leave it empty;
// anything meant to be reachable beyond localhost should set one.
type bearerTokenInterceptor struct {
	a2asrv.PassthroughCallInterceptor
	token string
}

var _ a2asrv.CallInterceptor = (*bearerTokenInterceptor)(nil)

func (b *bearerTokenInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	if b.token == "" {
		return ctx, nil, nil
	}
	values, _ := callCtx.ServiceParams().Get("Authorization")
	for _, v := range values {
		if matchesBearerToken(v, b.token) {
			callCtx.User = a2asrv.NewAuthenticatedUser(b.bindingUser(), nil)
			return ctx, nil, nil
		}
	}
	return ctx, nil, a2a.NewError(a2a.ErrUnauthenticated, "missing or invalid bearer token")
}

// bindingUser is a placeholder identity recorded on a successfully
// authenticated CallContext.User -- this package doesn't yet distinguish
// individual external callers, only "presented this binding's token."
func (b *bearerTokenInterceptor) bindingUser() string {
	return "a2a-peer"
}
