package launchresolve

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"
	"github.com/hollis-labs/go-agent-runtime/runtimekind"

	"github.com/hollis-labs/tether/internal/config"
)

// Package-level sentinel errors. Each query helper wraps the sentinel for
// its kind so callers branch precisely with errors.Is. An unresolvable id
// is always a HARD error — these helpers never fall back silently.
var (
	// ErrRuntimeBindingNotFound is returned by ResolveRuntimeBinding when
	// no runtime-binding record matches the runner id.
	ErrRuntimeBindingNotFound = errors.New("registry: runtime binding not found")

	// ErrAgentNotFound is returned by ResolveAgent when no agent-source
	// record matches the agent id.
	ErrAgentNotFound = errors.New("registry: agent source not found")

	// ErrMCPServerNotFound is returned by ResolveMCP when no mcp-server
	// record matches the server id.
	ErrMCPServerNotFound = errors.New("registry: mcp server not found")

	// ErrLaunchNotFound is returned by ResolveLaunch when no
	// execution-template or boot-spec record matches the launch id.
	ErrLaunchNotFound = errors.New("registry: launch not found")

	// ErrCatalogFileUnreadable is returned when a resolved record's
	// handle points at a catalog file that cannot be read or parsed.
	ErrCatalogFileUnreadable = errors.New("registry: catalog file unreadable")
)

// MCPServer is the resolved shape ResolveMCP returns. The live
// ~/.tether/catalog/mcp-servers/ files are Tether-native config.MCPServerEntry
// documents, not go-agent-launch MCPServerContract documents, so the helper
// returns the Tether-native type rather than fabricating a contract.
type MCPServer = config.MCPServerEntry

// LaunchResolution is the resolved shape ResolveLaunch returns. The kind
// vocabulary maps Tether catalog launches/ entries to the
// execution-template kind and boot-profiles/ entries to the boot-spec
// kind. A launch id may resolve to either; Kind reports which the lookup
// matched and Launch / BootProfileID carry the decoded value.
type LaunchResolution struct {
	// Kind is the registry kind the id resolved under: either
	// agentlaunch.RegistryKindExecutionTemplate (a launches/ entry) or
	// agentlaunch.RegistryKindBootSpec (a boot-profiles/ entry).
	Kind agentlaunch.RegistryKind

	// Ref is the registry object reference the lookup matched.
	Ref agentlaunch.RegistryObjectRef

	// Source is the handle the registry holds: the local catalog file
	// path plus its content digest (D2). Callers materialize richer
	// content from Source.FilePath on demand.
	Source agentlaunch.RegistrationSource

	// Launch is the decoded Tether-native launch document. It is populated
	// only when Kind is execution-template.
	Launch *config.Launch

	// BootProfileID is the boot-profile id. It is populated only when
	// Kind is boot-spec. The boot-profile body is Tether-native and is
	// left on disk (D2); callers load it from Source.FilePath.
	BootProfileID string
}

// ResolveRuntimeBinding resolves a runner id to a go-agent-launch
// RuntimeBinding by querying the runtime-binding kind. It is what feeds
// the Runtime input of the PlanFromLaunch bridge.
//
// An unresolvable runner id is a HARD error (ErrRuntimeBindingNotFound) —
// it never silently falls back to a default runtime.
//
// go-agent-launch friction: the live catalog providers/ files are
// Tether-native config.Provider documents, not RuntimeBindingContract
// documents, so agentlaunch.DecodeContract cannot decode them. This helper
// therefore hand-maps the Tether-native provider into a RuntimeBinding
// (provider brand + runtime kind + command/args), reusing the same
// EffectiveRuntimeKind / ProviderBrand mapping Tether's launch pipeline
// uses elsewhere so the resolved binding is consistent.
func (r *Registry) ResolveRuntimeBinding(runnerID string) (agentlaunch.RuntimeBinding, error) {
	rec, err := r.queryOne(agentlaunch.RegistryKindRuntimeBinding, runnerID, ErrRuntimeBindingNotFound)
	if err != nil {
		return agentlaunch.RuntimeBinding{}, err
	}

	var prov config.Provider
	if err := readCatalogYAML(rec.Source.FilePath, &prov); err != nil {
		return agentlaunch.RuntimeBinding{}, err
	}

	runtimeKind := mapRuntimeKind(prov.EffectiveRuntimeKind())
	if !runtimeKind.Valid() {
		return agentlaunch.RuntimeBinding{}, fmt.Errorf(
			"registry: runtime binding %q has unmappable runtime_kind %q",
			runnerID, prov.EffectiveRuntimeKind())
	}

	binding := agentlaunch.RuntimeBinding{
		Provider:    prov.ProviderBrand(),
		RuntimeKind: runtimeKind,
		Args:        prov.Args,
	}
	// Thread the permission posture onto the binding. go-agent-launch
	// v0.3.3 carries RuntimeBinding.Permission verbatim through
	// PlanFromLaunch -> providerplant onto the planted boot-dir content;
	// without it a headless claude launch hangs on the first approval
	// prompt. Only claude needs it — go-providers defaults codex's
	// approval_policy to "never" when empty.
	if binding.Provider == "claude" {
		binding.Permission = claudePermission(r.permissionMode)
	}
	if err := binding.Validate(); err != nil {
		return agentlaunch.RuntimeBinding{}, fmt.Errorf(
			"registry: runtime binding %q is invalid: %w", runnerID, err)
	}
	return binding, nil
}

// ResolveAgent resolves an agent id to a go-agent-launch AgentSpec by
// querying the agent-source kind. An unresolvable id is a HARD error
// (ErrAgentNotFound).
//
// Like ResolveRuntimeBinding, the live agents/ files are Tether-native
// config.Agent documents (not AgentSourceContract documents), so this
// helper hand-maps the Tether-native agent into an AgentSpec. The agent's
// roles flow through as routing labels so the returned spec is usable by
// the bridge.
func (r *Registry) ResolveAgent(agentID string) (agentlaunch.AgentSpec, error) {
	rec, err := r.queryOne(agentlaunch.RegistryKindAgentSource, agentID, ErrAgentNotFound)
	if err != nil {
		return agentlaunch.AgentSpec{}, err
	}

	var agent config.Agent
	if err := readCatalogYAML(rec.Source.FilePath, &agent); err != nil {
		return agentlaunch.AgentSpec{}, err
	}

	spec := agentlaunch.AgentSpec{
		ID:   agent.ID,
		Name: agent.Name,
	}
	if len(agent.Roles) > 0 {
		spec.Labels = map[string]string{"roles": joinRoles(agent.Roles)}
	}
	if spec.ID == "" {
		// The registry record name is the catalog id; fall back to it when
		// the file body omits id so the returned spec is always usable.
		spec.ID = rec.Meta.Ref.Name
	}
	return spec, nil
}

// ResolveMCP resolves an MCP server id to its catalog definition by
// querying the mcp-server kind. An unresolvable id is a HARD error
// (ErrMCPServerNotFound).
//
// The returned MCPServer is the Tether-native config.MCPServerEntry shape:
// the live mcp-servers/ catalog files are Tether-native, not go-agent-launch
// MCPServerContract documents, so the helper returns whatever the catalog
// file decodes to rather than fabricating a contract.
func (r *Registry) ResolveMCP(serverID string) (MCPServer, error) {
	rec, err := r.queryOne(agentlaunch.RegistryKindMCPServer, serverID, ErrMCPServerNotFound)
	if err != nil {
		return MCPServer{}, err
	}

	var srv config.MCPServerEntry
	if err := readCatalogYAML(rec.Source.FilePath, &srv); err != nil {
		return MCPServer{}, err
	}
	if srv.ID == "" {
		srv.ID = rec.Meta.Ref.Name
	}
	return srv, nil
}

// ResolveLaunch resolves a launch id over the execution-template and
// boot-spec kinds. Tether's catalog maps launches/ entries to the
// execution-template kind and boot-profiles/ entries to the boot-spec
// kind; ResolveLaunch tries execution-template first, then boot-spec, so a
// caller can resolve either without knowing which catalog subdir an id
// lives in. An unresolvable id is a HARD error (ErrLaunchNotFound).
func (r *Registry) ResolveLaunch(launchID string) (LaunchResolution, error) {
	if launchID == "" {
		return LaunchResolution{}, fmt.Errorf("%w: empty id", ErrLaunchNotFound)
	}

	// Prefer execution-template (a launches/ entry).
	if rec, err := r.queryOne(agentlaunch.RegistryKindExecutionTemplate, launchID, ErrLaunchNotFound); err == nil {
		var l config.Launch
		if err := readCatalogYAML(rec.Source.FilePath, &l); err != nil {
			return LaunchResolution{}, err
		}
		if l.ID == "" {
			l.ID = rec.Meta.Ref.Name
		}
		return LaunchResolution{
			Kind:   agentlaunch.RegistryKindExecutionTemplate,
			Ref:    rec.Meta.Ref,
			Source: rec.Source,
			Launch: &l,
		}, nil
	} else if !errors.Is(err, ErrLaunchNotFound) {
		// A non-not-found error (e.g. catalog integrity) must surface.
		return LaunchResolution{}, err
	}

	// Fall back to boot-spec (a boot-profiles/ entry).
	rec, err := r.queryOne(agentlaunch.RegistryKindBootSpec, launchID, ErrLaunchNotFound)
	if err != nil {
		return LaunchResolution{}, err
	}
	return LaunchResolution{
		Kind:          agentlaunch.RegistryKindBootSpec,
		Ref:           rec.Meta.Ref,
		Source:        rec.Source,
		BootProfileID: rec.Meta.Ref.Name,
	}, nil
}

// mapRuntimeKind maps a Tether config runtime-kind token onto the
// canonical agentlaunch.RuntimeKind enum. It mirrors the mapping used by
// internal/app and internal/bootexec so a registry-resolved binding is
// consistent with the rest of Tether's launch pipeline. The config "api"
// runtime has no agentlaunch equivalent and maps to the invalid zero
// value, which ResolveRuntimeBinding rejects as unmappable.
func mapRuntimeKind(kind string) agentlaunch.RuntimeKind {
	switch k := runtimekind.Parse(kind); k {
	case runtimekind.PTY, runtimekind.StreamingStdio, runtimekind.JSONRPCStdio, runtimekind.Subprocess:
		return k
	default:
		return agentlaunch.RuntimeKind("")
	}
}

// readCatalogYAML reads and unmarshals a catalog file into out. Catalog
// paths originate from the registry's own local catalog handle, never from
// a network boundary.
func readCatalogYAML(path string, out any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // catalog-sourced path from registry handle
	if err != nil {
		return fmt.Errorf("%w: read %s: %w", ErrCatalogFileUnreadable, path, err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: parse %s: %w", ErrCatalogFileUnreadable, path, err)
	}
	return nil
}

// joinRoles renders an agent's role slice as a comma-separated label
// value. AgentSpec.Labels is a flat string map, so the role list is
// flattened rather than dropped.
func joinRoles(roles []string) string {
	return strings.Join(roles, ",")
}

// claudePermission maps Tether's permission_mode knob (config vocabulary:
// "bypass" / "default" / "") onto the claude permission vocabulary the
// go-providers ClaudeAdapter expects (default / acceptEdits / plan /
// bypassPermissions). The mapping is faithful to operator intent —
// "bypass" -> bypassPermissions, "default"/"" -> default — and mirrors
// config.EffectivePermissionMode's empty->default rule.
//
// It always returns a non-empty value: an empty RuntimeBinding.Permission
// on a claude binding leaves a headless launch hanging on the first
// approval prompt (go-agent-launch v0.3.3 deliberately imposes no default).
func claudePermission(permissionMode string) string {
	switch permissionMode {
	case config.PermissionModeBypass:
		return "bypassPermissions"
	default:
		return "default"
	}
}
