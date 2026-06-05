package config

import (
	"fmt"
	"strings"

	"github.com/hollis-labs/agentkit/agentruntime/bootdir"
)

func (c *Catalog) Validate() error {
	for id, l := range c.Launches {
		if _, ok := c.Projects[l.Project]; !ok {
			return fmt.Errorf("launch %q references unknown project %q", id, l.Project)
		}
		if _, ok := c.Agents[l.Agent]; !ok {
			return fmt.Errorf("launch %q references unknown agent %q", id, l.Agent)
		}
		if _, ok := c.Providers[l.Provider]; !ok {
			return fmt.Errorf("launch %q references unknown provider %q", id, l.Provider)
		}
		if err := validateLaunchInjection(id, l.Injection); err != nil {
			return err
		}
	}
	for id, p := range c.Providers {
		typ := p.Type
		if typ == "" {
			typ = "cli"
		}
		switch p.EffectiveRuntimeKind() {
		case RuntimeKindPTY, RuntimeKindStreamingStdio, RuntimeKindJSONRPCStdio, RuntimeKindSubprocess, RuntimeKindAPI:
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
		if name := a.Permissions.DefaultSandbox; name != "" {
			if _, ok := c.SandboxProfiles[name]; !ok {
				return fmt.Errorf("agent %q references unknown sandbox profile %q", id, name)
			}
		}
		if m := a.Permissions.PermissionMode; !ValidPermissionMode(m) {
			return fmt.Errorf("agent %q has invalid permission_mode %q (want %q or %q)", id, m, PermissionModeDefault, PermissionModeBypass)
		}
	}
	return nil
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
