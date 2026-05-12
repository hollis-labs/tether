package app

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/chrispian/agent-mux/internal/bootgen"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/skills"
)

// CreateSessionInput is the v005-08 Agent Ops surface for session creation.
//
// Layered semantics — for the agent definition:
//   - AgentInline (JSON) wins if set
//   - AgentFile (YAML on disk) wins next
//   - Catalog entry resolved from LaunchID is the base
//
// For per-launch overrides, Override is parsed last as a thin JSON object
// merging selected fields onto the resolved plan.
//
// BootPromptOverride preserves the pre-v005-08 `boot_prompt` injection path:
// when non-empty it replaces the composed boot prompt verbatim, after all
// other v005-08 assembly has happened. Callers that just want the legacy
// behavior should populate LaunchID + BootPromptOverride and leave the new
// fields zero.
type CreateSessionInput struct {
	// LaunchID resolves the base launch profile from the catalog. Required.
	LaunchID string

	// BootPromptOverride replaces the composed boot prompt verbatim. Same as
	// the pre-v005-08 surface that backed `mux boot <profile>`.
	BootPromptOverride string

	// AgentFile is a filesystem path to an agent YAML matching the
	// config.Agent schema. Loaded and field-merged over the catalog agent.
	AgentFile string

	// AgentInline is a JSON-encoded agent definition. Field-merged with
	// highest precedence over AgentFile and catalog.
	AgentInline string

	// BootProfileFile is a filesystem path to a bootgen.Profile YAML.
	// When supplied, the profile's MCPServers populates MUX_MCP_SERVERS for
	// the spawned session.
	BootProfileFile string

	// Override is a JSON object applied last over the resolved plan.
	// Allowed fields:
	//   system_prompt: string  — replaces the composed boot prompt
	//   env:           {KEY: VALUE, ...} — merged onto plan.Env (Env mode)
	Override string
}

// LaunchOverride mirrors the JSON shape accepted in CreateSessionInput.Override.
// Defined as a named type so the JSON contract is auditable in one place.
type LaunchOverride struct {
	SystemPrompt string            `json:"system_prompt,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

// applyAgentOps mutates the resolved launch plan in place with the v005-08
// caller-provided agent / boot profile / override / skills compilation.
//
// Returns an error when any of the parse / merge steps fails. The plan is
// left in an undefined state on error — callers must abort.
func (s *Service) applyAgentOps(plan *launch.Plan, in CreateSessionInput) error {
	effectiveAgent, err := s.resolveEffectiveAgent(plan, in)
	if err != nil {
		return err
	}

	bootProfile, err := loadBootProfile(in.BootProfileFile)
	if err != nil {
		return err
	}

	composedPrompt, err := s.composeBootPrompt(plan, effectiveAgent)
	if err != nil {
		return err
	}

	composedPrompt, err = applyOverride(plan, composedPrompt, in.Override)
	if err != nil {
		return err
	}

	applyProviderOverrides(plan, effectiveAgent.ProviderOverrides)
	applyMCPAllowlist(plan, bootProfile)

	// BootPromptOverride wins last — explicit "give me exactly this prompt."
	if in.BootPromptOverride != "" {
		composedPrompt = in.BootPromptOverride
	}
	plan.BootPrompt = composedPrompt
	return nil
}

// resolveEffectiveAgent starts from the catalog-resolved agent and applies the
// Tier-2 file then inline merges (inline wins).
func (s *Service) resolveEffectiveAgent(plan *launch.Plan, in CreateSessionInput) (config.Agent, error) {
	baseAgent, ok := s.Catalog.Agents[plan.LogicalAgentID]
	if !ok {
		// The catalog should always have the agent the launch references;
		// missing here means the launch resolver let through an inconsistent
		// state. Treat as a hard error rather than silently falling back to
		// an empty Agent — downstream code expects Permissions to be set.
		return config.Agent{}, fmt.Errorf("agent %q referenced by launch %q not found in catalog", plan.LogicalAgentID, plan.LaunchID)
	}
	effectiveAgent := baseAgent

	if in.AgentFile != "" {
		data, err := os.ReadFile(in.AgentFile) //nolint:gosec // G304: operator-provided path
		if err != nil {
			return config.Agent{}, fmt.Errorf("agent_file: read %s: %w", in.AgentFile, err)
		}
		var fileAgent config.Agent
		if err := yaml.Unmarshal(data, &fileAgent); err != nil {
			return config.Agent{}, fmt.Errorf("agent_file: parse %s: %w", in.AgentFile, err)
		}
		mergeAgent(&effectiveAgent, fileAgent)
	}

	if in.AgentInline != "" {
		var inlineAgent config.Agent
		if err := json.Unmarshal([]byte(in.AgentInline), &inlineAgent); err != nil {
			return config.Agent{}, fmt.Errorf("agent_inline: parse: %w", err)
		}
		mergeAgent(&effectiveAgent, inlineAgent)
	}
	return effectiveAgent, nil
}

// loadBootProfile loads the caller-provided boot profile (file-only; no
// inline). Empty path returns the zero-value Profile and no error.
func loadBootProfile(path string) (bootgen.Profile, error) {
	if path == "" {
		return bootgen.Profile{}, nil
	}
	p, err := bootgen.LoadProfile(path)
	if err != nil {
		return bootgen.Profile{}, fmt.Errorf("boot_profile: %w", err)
	}
	return p, nil
}

// composeBootPrompt assembles the catalog boot prompt + SystemPrompt +
// AgentPrompt + provider-compiled skills sections into a single composed
// prompt. Returns the composed string; skill compilation errors surface
// unless they're "unsupported provider" (skip silently).
func (s *Service) composeBootPrompt(plan *launch.Plan, effectiveAgent config.Agent) (string, error) {
	var sb strings.Builder
	if plan.BootPrompt != "" {
		sb.WriteString(plan.BootPrompt)
		if !strings.HasSuffix(plan.BootPrompt, "\n") {
			sb.WriteString("\n")
		}
	}
	if effectiveAgent.SystemPrompt != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("# System\n\n")
		sb.WriteString(effectiveAgent.SystemPrompt)
		if !strings.HasSuffix(effectiveAgent.SystemPrompt, "\n") {
			sb.WriteString("\n")
		}
	}
	if effectiveAgent.AgentPrompt != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("# Agent\n\n")
		sb.WriteString(effectiveAgent.AgentPrompt)
		if !strings.HasSuffix(effectiveAgent.AgentPrompt, "\n") {
			sb.WriteString("\n")
		}
	}

	// Compile skills for the provider. Skills are appended to the BootPrompt
	// as text — once go-agent-sessions exposes an app-side PlantedFiles append
	// hook, this can shift to native per-file placement (.claude/skills/<id>.md
	// for Claude, AGENTS.md for Codex). Today it joins as inline sections.
	skillSet, err := s.loadEffectiveSkills(effectiveAgent.Skills)
	if err != nil {
		return "", err
	}
	if len(skillSet) > 0 {
		compiled, cerr := skills.CompileForProvider(plan.ProviderID, skillSet)
		if cerr != nil && !isUnsupportedProvider(cerr) {
			return "", fmt.Errorf("compile skills for %s: %w", plan.ProviderID, cerr)
		}
		for _, f := range compiled {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "<!-- %s -->\n", f.RelPath)
			sb.WriteString(f.Content)
			if !strings.HasSuffix(f.Content, "\n") {
				sb.WriteString("\n")
			}
		}
	}
	return sb.String(), nil
}

// applyOverride parses the caller's JSON override and applies it. SystemPrompt
// fully replaces the composed prompt; Env merges into plan.Env. Empty JSON is
// a no-op returning the composed prompt unchanged.
func applyOverride(plan *launch.Plan, composedPrompt, overrideJSON string) (string, error) {
	if overrideJSON == "" {
		return composedPrompt, nil
	}
	var ov LaunchOverride
	if err := json.Unmarshal([]byte(overrideJSON), &ov); err != nil {
		return "", fmt.Errorf("override: parse: %w", err)
	}
	if ov.SystemPrompt != "" {
		composedPrompt = ov.SystemPrompt
	}
	if len(ov.Env) > 0 {
		if plan.Env == nil {
			plan.Env = map[string]string{}
		}
		for k, v := range ov.Env {
			plan.Env[k] = v
		}
	}
	return composedPrompt, nil
}

// applyProviderOverrides applies the per-provider env / extra-args block from
// the resolved agent. No-op when the plan's ProviderID has no override entry.
func applyProviderOverrides(plan *launch.Plan, overrides map[string]config.ProviderOverride) {
	po, ok := overrides[plan.ProviderID]
	if !ok {
		return
	}
	if plan.Env == nil {
		plan.Env = map[string]string{}
	}
	for k, v := range po.Env {
		plan.Env[k] = v
	}
	if len(po.ExtraArgs) > 0 {
		plan.Args = append(plan.Args, po.ExtraArgs...)
	}
}

// applyMCPAllowlist threads the boot profile's MCP allowlist into MUX_MCP_SERVERS.
// Precedence: boot profile (this call) > catalog (launch/project). No-op when
// the boot profile is empty.
func applyMCPAllowlist(plan *launch.Plan, bootProfile bootgen.Profile) {
	if len(bootProfile.MCPServers) == 0 {
		return
	}
	if plan.Env == nil {
		plan.Env = map[string]string{}
	}
	plan.Env["MUX_MCP_SERVERS"] = strings.Join(bootProfile.MCPServers, ",")
}

// loadEffectiveSkills resolves a list of skill IDs against the layered
// discovery view, returning the loaded Skill structs in stable (input) order.
// Missing skill IDs are an error.
func (s *Service) loadEffectiveSkills(ids []string) ([]skills.Skill, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cat, err := config.Discover(config.DefaultLayers(s.CatalogRoot, s.CatalogRoot))
	if err != nil {
		return nil, fmt.Errorf("skills discovery: %w", err)
	}
	out := make([]skills.Skill, 0, len(ids))
	for _, id := range ids {
		path, ok := cat.SkillPaths[id]
		if !ok {
			return nil, fmt.Errorf("skill %q not found in any discovery layer", id)
		}
		s, err := skills.ParseFile(path.Path)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", id, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// mergeAgent applies higher-precedence fields from src onto dst. Empty / zero
// values in src do not clobber populated values in dst. List fields replace
// rather than concatenate — concatenation surprises far more often than it
// helps.
func mergeAgent(dst *config.Agent, src config.Agent) {
	if src.ID != "" {
		dst.ID = src.ID
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if len(src.Roles) > 0 {
		dst.Roles = append([]string(nil), src.Roles...)
	}
	if len(src.Skills) > 0 {
		dst.Skills = append([]string(nil), src.Skills...)
	}
	if len(src.ContextFiles) > 0 {
		dst.ContextFiles = append([]string(nil), src.ContextFiles...)
	}
	if len(src.BootFragments) > 0 {
		dst.BootFragments = append([]string(nil), src.BootFragments...)
	}
	// Permissions: explicit bools are always carried (a deliberate false
	// should be respected); DefaultSandbox replaces when non-empty.
	dst.Permissions.Network = src.Permissions.Network || dst.Permissions.Network
	if src.Permissions.DefaultSandbox != "" {
		dst.Permissions.DefaultSandbox = src.Permissions.DefaultSandbox
	}
	if src.SystemPrompt != "" {
		dst.SystemPrompt = src.SystemPrompt
	}
	if src.AgentPrompt != "" {
		dst.AgentPrompt = src.AgentPrompt
	}
	if len(src.ProviderOverrides) > 0 {
		if dst.ProviderOverrides == nil {
			dst.ProviderOverrides = map[string]config.ProviderOverride{}
		}
		// Sort keys for deterministic iteration in case anything observes order.
		keys := make([]string, 0, len(src.ProviderOverrides))
		for k := range src.ProviderOverrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dst.ProviderOverrides[k] = src.ProviderOverrides[k]
		}
	}
}

// isUnsupportedProvider returns true when err wraps skills.ErrUnsupportedProvider.
// Defined as a tiny helper so applyAgentOps stays readable; not exported.
func isUnsupportedProvider(err error) bool {
	for e := err; e != nil; {
		if e == skills.ErrUnsupportedProvider { //nolint:errorlint // sentinel check; chain unwrap below covers wrapped variants
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
