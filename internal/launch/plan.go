package launch

// Plan is the resolved launch contract the provider adapter consumes. Env
// fields describe a *policy* for composing the child's environment, not a
// pre-flattened result — the adapter applies the policy against the live
// os.Environ() at start time so fresh parent-env state is captured per
// launch and overrides are the only values persisted in the plan.
type Plan struct {
	LaunchID       string   `json:"launch_id"`
	ProjectID      string   `json:"project_id"`
	LogicalAgentID string   `json:"logical_agent_id"`
	ProviderID     string   `json:"provider_id"`
	ProviderBrand  string   `json:"provider_brand,omitempty"`
	RuntimeKind    string   `json:"runtime_kind,omitempty"`
	RepoRoot       string   `json:"repo_root"`
	WriteHome      string   `json:"write_home"`
	Command        string   `json:"command"`
	Args           []string `json:"args"`

	// Env holds explicit overrides (from the launch's overrides.env block).
	// The adapter applies these last, after composing the base environment
	// per EnvMode. Parent-inherited values are NOT materialized here.
	Env map[string]string `json:"env"`
	// EnvMode selects the env composition strategy; "merge" (default) or
	// "whitelist". See internal/provider/env.go for semantics.
	EnvMode string `json:"env_mode"`
	// EnvPassthrough lists keys pulled from the parent environment in
	// whitelist mode. Ignored in merge mode.
	EnvPassthrough []string `json:"env_passthrough,omitempty"`
	// EnvRedact lists keys to scrub from the parent environment in merge
	// mode. Ignored in whitelist mode.
	EnvRedact []string `json:"env_redact,omitempty"`

	BootPrompt string `json:"boot_prompt"`
	BootMode   string `json:"boot_mode"`
}
