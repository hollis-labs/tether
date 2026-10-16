package mcpgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// legacyPolicy is SessionPolicy exactly as it was before EditableAgents: the
// shape every already-saved session policy was hashed with.
type legacyPolicy struct {
	UpstreamOwnership string   `json:"upstream_ownership,omitempty"`
	ExtractRefs       bool     `json:"extract_refs,omitempty"`
	SessionID         string   `json:"session_id"`
	AgentID           string   `json:"agent_id"`
	Servers           []string `json:"servers"`
	Profile           *string  `json:"profile,omitempty"`
	LaunchProfile     *Profile `json:"launch_profile,omitempty"`
	ToolProfile       *Profile `json:"tool_profile,omitempty"`
	DiscoveryMode     *string  `json:"discovery_mode,omitempty"`
	Digest            string   `json:"digest"`
}

func TestEditableAgentsDigest(t *testing.T) {
	base := SessionPolicy{SessionID: "s1", AgentID: "a", Servers: []string{"one"}, ExtractRefs: true}

	// A policy without an allowlist keeps the digest it always had, so every
	// policy saved before this field existed still validates.
	legacy := legacyPolicy{SessionID: "s1", AgentID: "a", Servers: []string{"one"}, ExtractRefs: true}
	raw, _ := json.Marshal(legacy)
	sum := sha256.Sum256(raw)
	sealed := base.Seal()
	if sealed.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("a policy with no allowlist changed its digest: %s", sealed.Digest)
	}
	if err := sealed.Validate(); err != nil {
		t.Fatal(err)
	}

	// The allowlist is part of the digest, so a change is a different authority
	// (the MCP view fingerprint follows the digest).
	with := base
	with.EditableAgents = []string{"worker", "reviewer"}
	withSealed := with.Seal()
	if withSealed.Digest == sealed.Digest {
		t.Fatal("adding an allowlist did not change the digest")
	}
	other := base
	other.EditableAgents = []string{"worker", "planner"}
	if other.Seal().Digest == withSealed.Digest {
		t.Fatal("a different allowlist has the same digest")
	}

	// The same set is the same authority however it is spelled.
	spelled := base
	spelled.EditableAgents = []string{"reviewer", "worker", "worker", ""}
	spelledSealed := spelled.Seal()
	if spelledSealed.Digest != withSealed.Digest || !slices.Equal(spelledSealed.EditableAgents, []string{"reviewer", "worker"}) {
		t.Fatalf("order/duplicates/empty ids changed the policy: %+v", spelledSealed)
	}
	// An all-empty list is no allowlist at all.
	empty := base
	empty.EditableAgents = []string{"", ""}
	if got := empty.Seal(); got.EditableAgents != nil || got.Digest != sealed.Digest {
		t.Fatalf("empty allowlist: %+v", got)
	}

	// Seal copies: later edits to the caller's slice do not reach the policy.
	src := []string{"b", "a"}
	cp := SessionPolicy{SessionID: "s1", AgentID: "a", Servers: []string{}, EditableAgents: src}.Seal()
	src[0] = "tampered"
	if cp.EditableAgents[0] != "a" || cp.EditableAgents[1] != "b" {
		t.Fatalf("policy aliases the caller's slice: %v", cp.EditableAgents)
	}
}

func TestEditableAgentsValidate(t *testing.T) {
	good := SessionPolicy{SessionID: "s1", AgentID: "a", Servers: []string{}, EditableAgents: []string{"a", "b"}}.Seal()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	// Tampering with the stored list invalidates the policy (digest), and a
	// list that is not canonical is refused even if the digest were recomputed.
	tampered := good
	tampered.EditableAgents = []string{"a", "b", "c"}
	if err := tampered.Validate(); !errors.Is(err, ErrInvalidSessionMCPPolicy) {
		t.Fatalf("tampered allowlist accepted: %v", err)
	}
	for name, ids := range map[string][]string{"unsorted": {"b", "a"}, "duplicate": {"a", "a"}, "empty id": {"a", ""}} {
		p := SessionPolicy{SessionID: "s1", AgentID: "a", Servers: []string{}, EditableAgents: ids}
		p.Digest = p.hash()
		if err := p.Validate(); !errors.Is(err, ErrInvalidSessionMCPPolicy) {
			t.Errorf("%s allowlist accepted: %v", name, err)
		}
	}
}
