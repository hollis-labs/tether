// Package agent defines the durable LogicalAgent identity model used by
// Agent Mux. A LogicalAgent is the stable handle for an agent across
// any number of ephemeral RuntimeSessions; policies, checkpoint rules,
// and hot/cold behaviour all attach here.
//
// v0.0.2 carries only id/role/name plus TEXT columns for forward
// compatibility; policy/capability/checkpoint fields are reserved and
// stay empty until v0.1 introduces semantics (see ADR 0003 and
// context-pack §02).
package agent

// LogicalAgent mirrors the logical_agents row. String fields map to
// nullable TEXT columns; empty string ↔ SQL NULL for v0.0.2 ergonomics.
type LogicalAgent struct {
	ID               string
	Role             string
	Name             string
	Responsibilities string
	Capabilities     string
	MemoryScopes     string
	PoliciesJSON     string
	PermittedTools   string
	EscalationRules  string
	CheckpointPolicy string
	HotColdPolicy    string
	CreatedAt        string
	UpdatedAt        string
}
