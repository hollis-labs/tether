package launch

type Plan struct {
	LaunchID   string            `json:"launch_id"`
	ProjectID  string            `json:"project_id"`
	AgentID    string            `json:"agent_id"`
	ProviderID string            `json:"provider_id"`
	RepoRoot   string            `json:"repo_root"`
	WriteHome  string            `json:"write_home"`
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	BootPrompt string            `json:"boot_prompt"`
	BootMode   string            `json:"boot_mode"`
}
