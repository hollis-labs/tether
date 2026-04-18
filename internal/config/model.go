package config

type Global struct {
	Version string       `yaml:"version"`
	Catalog CatalogRoots `yaml:"catalog"`
}

type CatalogRoots struct {
	Roots    CatalogPaths `yaml:"roots"`
	Defaults Defaults     `yaml:"defaults"`
}

type CatalogPaths struct {
	Projects  string `yaml:"projects"`
	Agents    string `yaml:"agents"`
	Providers string `yaml:"providers"`
	Launches  string `yaml:"launches"`
	Boot      string `yaml:"boot"`
}

type Defaults struct {
	WorkspaceRoot string `yaml:"workspace_root"`
	StateDB       string `yaml:"state_db"`
	TempRoot      string `yaml:"temp_root"`
}

type Project struct {
	ID            string        `yaml:"id"`
	Name          string        `yaml:"name"`
	RepoRoot      string        `yaml:"repo_root"`
	TrackingRoot  string        `yaml:"tracking_root"`
	KnowledgeBase []string      `yaml:"knowledge_base"`
	BootFragments []string      `yaml:"boot_fragments"`
	Workspace     WorkspaceSpec `yaml:"workspace"`
}

type WorkspaceSpec struct {
	DefaultMode  string `yaml:"default_mode"`
	WorktreeBase string `yaml:"worktree_base"`
	SessionRoot  string `yaml:"session_root"`
}

type Agent struct {
	ID            string           `yaml:"id"`
	Name          string           `yaml:"name"`
	Roles         []string         `yaml:"roles"`
	Skills        []string         `yaml:"skills"`
	ContextFiles  []string         `yaml:"context_files"`
	BootFragments []string         `yaml:"boot_fragments"`
	Permissions   AgentPermissions `yaml:"permissions"`
}

type AgentPermissions struct {
	Network        bool   `yaml:"network"`
	DefaultSandbox string `yaml:"default_sandbox"`
}

type Provider struct {
	ID        string        `yaml:"id"`
	Type      string        `yaml:"type"`
	Command   string        `yaml:"command"`
	Args      []string      `yaml:"args"`
	Bootstrap BootstrapSpec `yaml:"bootstrap"`
	Env       ProviderEnv   `yaml:"env"`
}

type BootstrapSpec struct {
	Mode         string `yaml:"mode"`
	PromptPrefix string `yaml:"prompt_prefix"`
}

type ProviderEnv struct {
	Passthrough []string `yaml:"passthrough"`
}

type Launch struct {
	ID        string          `yaml:"id"`
	Project   string          `yaml:"project"`
	Agent     string          `yaml:"agent"`
	Provider  string          `yaml:"provider"`
	Workspace LaunchWorkspace `yaml:"workspace"`
	Prompt    PromptSpec      `yaml:"prompt"`
	Overrides LaunchOverrides `yaml:"overrides"`
}

type LaunchWorkspace struct {
	Mode         string `yaml:"mode"`
	WorktreeName string `yaml:"worktree_name"`
	WriteHome    string `yaml:"write_home"`
}

type PromptSpec struct {
	IncludeProjectBoot   bool `yaml:"include_project_boot"`
	IncludeAgentBoot     bool `yaml:"include_agent_boot"`
	IncludeKnowledgeBase bool `yaml:"include_knowledge_base"`
}

type LaunchOverrides struct {
	Env map[string]string `yaml:"env"`
}

type Catalog struct {
	Global    Global
	Projects  map[string]Project
	Agents    map[string]Agent
	Providers map[string]Provider
	Launches  map[string]Launch
}
