package identity

import "context"

// Sender classes and evidence for a turn input (CW-20261021-0010). The values are
// persisted (turn_inputs) and published on routed outputs; they are spelled once
// here and mirrored by the store's constants.
const (
	InputClassOperator = "operator"
	InputClassAgent    = "agent"
	InputClassSystem   = "system"
	InputClassUnknown  = "unknown"

	InputBasisPrincipal = "authenticated_principal"
	InputBasisNone      = "none"
	// InputBasisSharedOperator marks input submitted with the shared operator
	// credential, which a human at the CLI and a local agent both hold.
	InputBasisSharedOperator = "shared_operator_credential"
)

// ClassifyInputSender maps the caller's authenticated identity to the class of an
// input it submits. Only a verified principal is evidence; everything else is
// unknown. Any doubt resolves to unknown, never to operator. DEV policy, to be
// revisited at release posture.
//
//   - no verified principal (anonymous, observe-mode ?as=, identity off): unknown;
//   - the local operator credential: unknown (basis shared_operator_credential). A
//     human at the CLI and an agent on the operator-authorized connection are
//     indistinguishable behind it; per-agent credentials are the real fix
//     (CW-20261020-0047). Cost: CLI turns typed with the operator token are unknown;
//   - a paired device holding an interactive scope: operator (the only operator);
//   - a paired device: operator only if it holds an interactive scope (operate);
//     a device holding only the host-credential worker scope is system; any other
//     device is unknown;
//   - a session or agent principal: agent; a service principal: system;
//   - anything else: unknown.
func ClassifyInputSender(p Principal, ok bool) (urn, class, basis string) {
	if !ok || !p.Verified {
		return "", InputClassUnknown, InputBasisNone
	}
	switch {
	case IsOperatorCredential(p):
		return p.ID, InputClassUnknown, InputBasisSharedOperator
	case p.Kind == "device":
		switch {
		case ScopeGranted(p.Scopes, ScopeOperate):
			return p.ID, InputClassOperator, InputBasisPrincipal
		case ScopeGranted(p.Scopes, ScopeWorker):
			return p.ID, InputClassSystem, InputBasisPrincipal
		}
		return p.ID, InputClassUnknown, InputBasisPrincipal
	case p.Kind == "session" || p.Kind == "agent":
		return p.ID, InputClassAgent, InputBasisPrincipal
	case p.Kind == "service":
		return p.ID, InputClassSystem, InputBasisPrincipal
	}
	return p.ID, InputClassUnknown, InputBasisPrincipal
}

type inputSourceKey struct{}

// WithInputSource tags a request context with the route an input arrived by
// (chat_send, session_turn, mcp_send_turn). The sender itself is never taken from
// the context tag: it is classified from the authenticated principal.
func WithInputSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, inputSourceKey{}, source)
}

// InputSourceFrom returns the route tag, or "".
func InputSourceFrom(ctx context.Context) string {
	s, _ := ctx.Value(inputSourceKey{}).(string)
	return s
}

// InputOrigin is the provenance of a prompt that enters a session as part of its
// creation (a chat send that starts a session). It is set by the server from the
// authenticated request, never from a request body.
type InputOrigin struct {
	Source    string
	SenderURN string
	Class     string
	Basis     string
}
