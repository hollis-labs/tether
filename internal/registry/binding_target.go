package registry

// binding_target.go — T06 (messaging vNext, CW-20260906-0037): constructs
// the canonical RuntimeBinding target URN for a Tether logical agent.
//
// resolveNotifySession's pre-T06 legacy fallback (T05) already matches a
// msg://agent/... wake target against SessionRow.LogicalAgentID by ID alone
// -- it does not check the caller-supplied authority segment at all (any
// caller-chosen authority string works, since sender/recipient identity is
// self-asserted per ADR 0045). RuntimeBinding lookups preserve that same
// ID-only-matters contract by keying the binding table on ONE fixed,
// Tether-owned authority (defaultAuthority, "agent-mux" -- the same
// constant id.go already uses for minted registry URNs) rather than
// whatever authority a given caller happened to type. This sidesteps a
// much larger identity-reconciliation question (whether/how caller-chosen
// authorities should ever have to match a canonical one) that is out of
// T06's scope -- T06 needs one consistent internal key, not a resolution
// of every possible caller convention.

// LogicalAgentBindingTarget returns the canonical msg://agent/agent-mux/
// <logicalAgentID> URN used as a RuntimeBinding's target for a Tether
// logical agent.
func LogicalAgentBindingTarget(logicalAgentID string) string {
	return urnScheme + urnKindAgent + "/" + defaultAuthority + "/" + logicalAgentID
}
