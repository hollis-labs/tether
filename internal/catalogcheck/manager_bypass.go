package catalogcheck

import (
	"fmt"
	"sort"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

// ManagerBypass warns about each manager agent whose effective permission_mode
// (its own, else defaults.permission_mode) is bypass. Managers are meant to run
// with permission_mode: default and an MCP-only allowlist (CW-20261012-0123).
// dev posture 2026-10-14: restore as error before release; see Torque Security project PRJ-20261014-0001 and CW-20261013-0053
func ManagerBypass(cat *config.Catalog) []Result {
	ids := make([]string, 0, len(cat.Agents))
	for id := range cat.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Result
	for _, id := range ids {
		a := cat.Agents[id]
		if a.IsManager() && config.EffectivePermissionMode(cat.Global, a) == config.PermissionModeBypass {
			out = append(out, Warn("manager-bypass", fmt.Sprintf("agent %q has role %q and resolves to permission_mode %q; managers should not run with blanket bypass", id, launchprofile.RoleManager, config.PermissionModeBypass), "set permissions.permission_mode: default and an MCP-only provider_overrides allowlist (accepted for now under the 2026-10-14 dev posture)"))
		}
	}
	return out
}
