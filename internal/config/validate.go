package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hollis-labs/agentkit/agentruntime/bootdir"
	"github.com/hollis-labs/go-sandbox/sandbox"
)

// ErrUnknownSandboxProfile means an agent names a sandbox profile the
// catalog does not define. A launch of that agent is refused rather than
// run with no sandbox (CW-20261001-0130).
var ErrUnknownSandboxProfile = errors.New("unknown sandbox profile")

// Validate checks the whole catalog at load. It does not fail on an agent
// that names an unknown sandbox profile: that would take down every launch
// for one bad agent entry. SandboxIssues reports those, and ValidateLaunch
// and AgentSandbox refuse the affected launches.
func (c *Catalog) Validate() error {
	for id := range c.Launches {
		if err := c.validateLaunchRefs(id); err != nil {
			return err
		}
	}
	for id, p := range c.Providers {
		typ := p.Type
		if typ == "" {
			typ = "cli"
		}
		switch p.EffectiveRuntimeKind() {
		case RuntimeKindPTY, RuntimeKindStreamingStdio, RuntimeKindJSONRPCStdio, RuntimeKindSubprocess, RuntimeKindAPI, RuntimeKindACPStdio:
		default:
			return fmt.Errorf("provider %q has unsupported runtime_kind %q", id, p.EffectiveRuntimeKind())
		}
		switch typ {
		case "cli":
			// Empty command is valid: the adapter's Detect() resolves the binary
			// at launch time via $<BRAND>_CLI_PATH or exec.LookPath.
		case "cli-goprovider":
			if p.Adapter == "" {
				return fmt.Errorf("provider %q (cli-goprovider) missing adapter field", id)
			}
			switch p.Adapter {
			case "claude", "codex":
			default:
				return fmt.Errorf("provider %q (cli-goprovider) has unsupported adapter %q", id, p.Adapter)
			}
		case "api":
			// "api" type (api-stub, future API-backed) has no command requirement.
		default:
			return fmt.Errorf("provider %q has unsupported type %q", id, typ)
		}
	}
	if m := c.Global.Catalog.Defaults.PermissionMode; !ValidPermissionMode(m) {
		return fmt.Errorf("global defaults.permission_mode %q is invalid (want %q or %q)", m, PermissionModeDefault, PermissionModeBypass)
	}
	if err := validateAIConfig(c.Global.AI); err != nil {
		return err
	}
	if err := c.Global.Federation.Validate(); err != nil {
		return err
	}
	for id, a := range c.Agents {
		if m := a.Permissions.PermissionMode; !ValidPermissionMode(m) {
			return fmt.Errorf("agent %q has invalid permission_mode %q (want %q or %q)", id, m, PermissionModeDefault, PermissionModeBypass)
		}
	}
	return nil
}

// ValidateLaunch checks one launch entry against the rest of the catalog:
// the project, agent and provider it names must exist, its injection block
// must be well formed, and its agent's sandbox profile, if it names one,
// must be defined. The launch path runs it for the launch being created.
func (c *Catalog) ValidateLaunch(id string) error {
	if err := c.validateLaunchRefs(id); err != nil {
		return err
	}
	if _, _, err := c.AgentSandbox(c.Launches[id].Agent); err != nil {
		return fmt.Errorf("launch %q: %w", id, err)
	}
	return nil
}

// AgentSandbox resolves agentID's default sandbox profile. ok is false when
// the agent names none, or is not in the catalog; the error wraps
// ErrUnknownSandboxProfile when it names a profile the catalog does not
// define.
func (c *Catalog) AgentSandbox(agentID string) (profile sandbox.Profile, ok bool, err error) {
	a, found := c.Agents[agentID]
	if !found {
		return sandbox.Profile{}, false, nil
	}
	return c.SandboxProfile(agentID, a.Permissions.DefaultSandbox)
}

// SandboxProfile resolves a sandbox profile name an agent names. An empty
// name is no sandbox (ok false, no error), as it has always been; a name
// the catalog does not define is an error wrapping ErrUnknownSandboxProfile.
func (c *Catalog) SandboxProfile(agentID, name string) (profile sandbox.Profile, ok bool, err error) {
	if name == "" {
		return sandbox.Profile{}, false, nil
	}
	sp, found := c.SandboxProfiles[name]
	if !found {
		return sandbox.Profile{}, false, fmt.Errorf("%w: agent %q names sandbox profile %q, which is not defined under sandbox-profiles/; refusing to launch it without a sandbox",
			ErrUnknownSandboxProfile, agentID, name)
	}
	return sp, true, nil
}

// SandboxIssues returns one error for each agent that names a sandbox
// profile the catalog does not define, sorted by agent id.
func (c *Catalog) SandboxIssues() []error {
	ids := make([]string, 0, len(c.Agents))
	for id := range c.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var issues []error
	for _, id := range ids {
		if _, _, err := c.AgentSandbox(id); err != nil {
			issues = append(issues, err)
		}
	}
	return issues
}

// validateLaunchRefs is ValidateLaunch without the sandbox check: the
// project, agent and provider must exist and the injection must be well
// formed. Validate runs it for every launch at load.
func (c *Catalog) validateLaunchRefs(id string) error {
	l, ok := c.Launches[id]
	if !ok {
		return fmt.Errorf("launch %q not in catalog", id)
	}
	if _, ok := c.Projects[l.Project]; !ok {
		return fmt.Errorf("launch %q references unknown project %q", id, l.Project)
	}
	if _, ok := c.Agents[l.Agent]; !ok {
		return fmt.Errorf("launch %q references unknown agent %q", id, l.Agent)
	}
	if _, ok := c.Providers[l.Provider]; !ok {
		return fmt.Errorf("launch %q references unknown provider %q", id, l.Provider)
	}
	return validateLaunchInjection(id, l.Injection)
}

func validateAIConfig(ai AIConfig) error {
	if len(ai.Providers) == 0 {
		return nil
	}
	if err := validateAIPolicyConfig("global ai.policy", ai.Policy); err != nil {
		return err
	}
	if hasAIUsageBudgetConfig(ai.Policy.UsageBudget) && ai.Policy.UsageBudget.MaxCostUSD == nil {
		return fmt.Errorf("global ai.policy usage_budget.max_cost_usd is required when usage_budget is set")
	}

	seen := map[string]struct{}{}
	enabled := map[string]struct{}{}
	enabledProviders := map[string]AIProviderConfig{}
	for i, p := range ai.Providers {
		if p.ID == "" {
			return fmt.Errorf("global ai.providers[%d] missing id", i)
		}
		if _, ok := seen[p.ID]; ok {
			return fmt.Errorf("global ai.providers has duplicate id %q", p.ID)
		}
		seen[p.ID] = struct{}{}
		if !p.Enabled {
			continue
		}
		enabled[p.ID] = struct{}{}
		enabledProviders[p.ID] = p
		if err := validateAIPolicyConfig(fmt.Sprintf("global ai.providers[%d] (%s) policy", i, p.ID), p.Policy); err != nil {
			return err
		}
		if hasAIUsageBudgetConfig(p.Policy.UsageBudget) && coalesceFloat64Ptr(p.Policy.UsageBudget.MaxCostUSD, ai.Policy.UsageBudget.MaxCostUSD) == nil {
			return fmt.Errorf("global ai.providers[%d] (%s) policy usage_budget.max_cost_usd is required when usage_budget is set", i, p.ID)
		}
		models := p.EffectiveModels()
		switch p.Type {
		case "anthropic", "openai", "gemini":
			if p.SecretRef == "" {
				return fmt.Errorf("global ai.providers[%d] (%s) missing secret_ref", i, p.ID)
			}
			if len(models) == 0 {
				return fmt.Errorf("global ai.providers[%d] (%s) missing model or models", i, p.ID)
			}
		case "openai-compatible":
			if p.BaseURL == "" {
				return fmt.Errorf("global ai.providers[%d] (%s) missing base_url", i, p.ID)
			}
			if len(models) == 0 {
				return fmt.Errorf("global ai.providers[%d] (%s) missing model or models", i, p.ID)
			}
		case "":
			return fmt.Errorf("global ai.providers[%d] (%s) missing type", i, p.ID)
		default:
			return fmt.Errorf("global ai.providers[%d] (%s) has unsupported type %q", i, p.ID, p.Type)
		}
		if p.DefaultModel != "" && !containsString(models, p.DefaultModel) {
			return fmt.Errorf("global ai.providers[%d] (%s) default_model %q is not present in model list", i, p.ID, p.DefaultModel)
		}
	}

	for _, id := range ai.Routing.DefaultProviderOrder {
		if _, ok := enabled[id]; !ok {
			return fmt.Errorf("global ai.routing.default_provider_order references unknown or disabled provider %q", id)
		}
	}
	for i, route := range ai.Routing.Routes {
		if route.Provider == "" {
			return fmt.Errorf("global ai.routing.routes[%d] missing provider", i)
		}
		p, ok := enabledProviders[route.Provider]
		if !ok {
			return fmt.Errorf("global ai.routing.routes[%d] references unknown or disabled provider %q", i, route.Provider)
		}
		if route.Model == "" {
			return fmt.Errorf("global ai.routing.routes[%d] missing model", i)
		}
		if !containsString(p.EffectiveModels(), route.Model) {
			return fmt.Errorf("global ai.routing.routes[%d] model %q is not configured for provider %q", i, route.Model, route.Provider)
		}
		if err := validateAIPolicyConfig(
			fmt.Sprintf("global ai.routing.routes[%d]", i),
			AIPolicyConfig{
				AllowReasoning:   route.AllowReasoning,
				AllowTools:       route.AllowTools,
				AllowAttachments: route.AllowAttachments,
				MaxOutputTokens:  route.MaxOutputTokens,
				MaxCostUSD:       route.MaxCostUSD,
				UsageBudget:      route.UsageBudget,
			},
		); err != nil {
			return err
		}
		if hasAIUsageBudgetConfig(route.UsageBudget) && coalesceFloat64Ptr(route.UsageBudget.MaxCostUSD, coalesceFloat64Ptr(p.Policy.UsageBudget.MaxCostUSD, ai.Policy.UsageBudget.MaxCostUSD)) == nil {
			return fmt.Errorf("global ai.routing.routes[%d] usage_budget.max_cost_usd is required when usage_budget is set", i)
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validateAIPolicyConfig(prefix string, policy AIPolicyConfig) error {
	if policy.MaxOutputTokens != nil && *policy.MaxOutputTokens <= 0 {
		return fmt.Errorf("%s max_output_tokens must be > 0", prefix)
	}
	if policy.MaxCostUSD != nil && *policy.MaxCostUSD <= 0 {
		return fmt.Errorf("%s max_cost_usd must be > 0", prefix)
	}
	if err := validateAIUsageBudgetPolicyConfig(prefix, policy.UsageBudget); err != nil {
		return err
	}
	return nil
}

func validateAIUsageBudgetPolicyConfig(prefix string, budget AIUsageBudgetPolicyConfig) error {
	if budget.MaxCostUSD != nil && *budget.MaxCostUSD <= 0 {
		return fmt.Errorf("%s usage_budget.max_cost_usd must be > 0", prefix)
	}
	switch budget.Window {
	case "", "day", "month":
	default:
		return fmt.Errorf("%s usage_budget.window %q is invalid (want \"day\" or \"month\")", prefix, budget.Window)
	}
	switch budget.Scope {
	case "", "total", "caller", "session":
	default:
		return fmt.Errorf("%s usage_budget.scope %q is invalid (want \"total\", \"caller\", or \"session\")", prefix, budget.Scope)
	}
	return nil
}

func hasAIUsageBudgetConfig(budget AIUsageBudgetPolicyConfig) bool {
	return budget.MaxCostUSD != nil || budget.Window != "" || budget.Scope != ""
}

func coalesceFloat64Ptr(primary, fallback *float64) *float64 {
	if primary != nil {
		return primary
	}
	return fallback
}

func validateLaunchInjection(launchID string, in LaunchInjection) error {
	for i, f := range in.NativeFiles {
		if f.Content != "" && f.Source != "" {
			return fmt.Errorf("launch %q injection.native_files[%d] sets both content and source", launchID, i)
		}
		kind := f.Kind
		if kind == "" {
			kind = "raw"
		}
		switch kind {
		case "raw":
			if f.RelPath == "" {
				return fmt.Errorf("launch %q injection.native_files[%d] missing rel_path", launchID, i)
			}
			if !isSafeInjectedRelPath(f.RelPath) {
				return fmt.Errorf("launch %q injection.native_files[%d] has unsafe rel_path %q", launchID, i, f.RelPath)
			}
		case "skill":
			if f.ID == "" {
				return fmt.Errorf("launch %q injection.native_files[%d] missing id", launchID, i)
			}
		default:
			return fmt.Errorf("launch %q injection.native_files[%d] has unsupported kind %q", launchID, i, f.Kind)
		}
	}
	for i, f := range in.BootDirOverlay {
		if f.RelPath == "" {
			return fmt.Errorf("launch %q injection.boot_dir_overlay[%d] missing rel_path", launchID, i)
		}
		if !isSafeInjectedRelPath(f.RelPath) {
			return fmt.Errorf("launch %q injection.boot_dir_overlay[%d] has unsafe rel_path %q", launchID, i, f.RelPath)
		}
		if f.Content != "" && f.Source != "" {
			return fmt.Errorf("launch %q injection.boot_dir_overlay[%d] sets both content and source", launchID, i)
		}
	}
	return nil
}

func isSafeInjectedRelPath(rel string) bool {
	if strings.HasPrefix(rel, "~") {
		return false
	}
	return bootdir.ValidateRelPath(rel) == nil
}
