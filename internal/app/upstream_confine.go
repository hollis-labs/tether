package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
)

// ConfineMCPPlant protects the whole local MCP process tree, rather than
// trusting individual tools to reject writes. Codex spawns its MCP servers
// outside its own sandbox; wrapping only its proxy avoids nesting bubblewrap
// around Codex itself or around Claude's already-wrapped proxy. A failed
// wrapper never falls back to running the proxy on the host.
//
// This is control-plane write protection, not workspace confinement: upstreams
// retain host reads, network access and writes outside the protected trees.
// HTTP/SSE servers cannot inherit this namespace and are refused separately
// by a protected proxy. The Codex config/caller-identity guard remains dormant.
func ConfineMCPPlant(plan *launch.Plan, command string, args, protected []string) (string, []string, error) {
	command, args, _, err := confineMCPPlant(plan, command, args, protected, false)
	return command, args, err
}

// AllowUnenforcedUserServiceManagerEnv names the tetherd environment variable
// that lets a darwin host launch Codex with the MCP proxy confined WITHOUT the
// user-service-manager (launchd user agent) denial the Linux profile enforces,
// because the darwin sandbox cannot enforce it. Off by default, so darwin stays
// fail-closed unless the operator sets it to "1" or "true". It has no effect on
// other platforms. Every launch that takes the downgrade logs a WARN and
// records a sandbox_rule_unenforced session event.
const AllowUnenforcedUserServiceManagerEnv = "TETHER_SANDBOX_ALLOW_UNENFORCED_USER_SERVICE_MANAGER"

// sandboxRuleUnenforcedEvent is the session event kind recorded whenever a
// launch runs with a sandbox rule the platform cannot enforce.
const sandboxRuleUnenforcedEvent = "sandbox_rule_unenforced"

// allowUnenforcedUserServiceManager reports whether the operator opted in to
// the darwin downgrade for goos and the daemon environment.
func allowUnenforcedUserServiceManager(goos string, getenv func(string) string) bool {
	if goos != "darwin" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv(AllowUnenforcedUserServiceManagerEnv))) {
	case "1", "true":
		return true
	}
	return false
}

// confineMCPPlant is ConfineMCPPlant with the explicit darwin downgrade: when
// allowUnenforcedUSM is set, the DenyUserServiceManager rule is dropped from
// the profile and downgraded reports it, so the caller can log and audit it.
func confineMCPPlant(plan *launch.Plan, command string, args, protected []string, allowUnenforcedUSM bool) (_ string, _ []string, downgraded bool, _ error) {
	if plan == nil || plan.ProviderBrand != "codex" || len(protected) == 0 {
		return command, args, false, nil
	}
	// #nosec G204 -- daemon-selected proxy executable and planted arguments.
	cmd := exec.Command(command, args...)
	profile := sandbox.Profile{
		ID: "tether-mcp-control-plane", HostFilesystem: true,
		Net: true, Subprocess: true, DenyUserServiceManager: !allowUnenforcedUSM,
		FS: sandbox.FSSpec{Protect: protected},
	}
	cleanup, err := sandbox.Apply(cmd, profile, plan.RepoRoot)
	if err != nil {
		return "", nil, false, fmt.Errorf("confine Codex MCP proxy: %w", err)
	}
	cleanup() // protect-only has no runtime helper or temporary resources
	return cmd.Path, cmd.Args[1:], allowUnenforcedUSM, nil
}

func confinedMCPEnv(plan *launch.Plan, protected []string) map[string]string {
	env := tetherEnvMap(plan.Env)
	if plan.ProviderBrand == "codex" && len(protected) > 0 {
		env[config.MCPConfineRemoteEnv] = "1"
	}
	return env
}

// recordUnenforcedUserServiceManager makes the downgrade loud and auditable:
// a WARN in the daemon log and a sandbox_rule_unenforced event on the session.
func (s *Service) recordUnenforcedUserServiceManager(ctx context.Context, sessionID string) {
	log.Printf("WARN: sandbox: session %s: DenyUserServiceManager is NOT enforced on darwin; the Codex MCP proxy can reach the launchd user service manager (enabled by %s)", sessionID, AllowUnenforcedUserServiceManagerEnv)
	raw, _ := json.Marshal(map[string]string{
		"rule":    "DenyUserServiceManager",
		"profile": "tether-mcp-control-plane",
		"reason":  "darwin sandbox cannot enforce it; operator opted in via " + AllowUnenforcedUserServiceManagerEnv,
	})
	var err error
	if s.Bus == nil {
		_, _, err = s.Store.InsertEventContext(ctx, events.ScopeSession, sessionID, sandboxRuleUnenforcedEvent, string(raw))
	} else {
		err = s.Bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: sessionID, Kind: sandboxRuleUnenforcedEvent, PayloadJSON: string(raw)})
	}
	if err != nil {
		log.Printf("WARN: sandbox: session %s: record %s event: %v", sessionID, sandboxRuleUnenforcedEvent, err)
	}
}
