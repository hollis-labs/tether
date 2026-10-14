package catalogcheck

import (
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestManagerBypassWarns(t *testing.T) {
	mgr := func(mode string) config.Agent {
		return config.Agent{ID: "mgr", Roles: []string{launchprofile.RoleManager}, Permissions: launchprofile.AgentPermissions{PermissionMode: mode}}
	}
	bypassGlobal := config.Global{}
	bypassGlobal.Catalog.Defaults.PermissionMode = config.PermissionModeBypass
	cases := []struct {
		name   string
		global config.Global
		agent  config.Agent
		warn   bool
	}{
		{"explicit bypass", config.Global{}, mgr(config.PermissionModeBypass), true},
		{"inherited global bypass", bypassGlobal, mgr(""), true},
		{"explicit default over global bypass", bypassGlobal, mgr(config.PermissionModeDefault), false},
		{"unset", config.Global{}, mgr(""), false},
		{"worker may bypass", config.Global{}, config.Agent{ID: "mgr", Roles: []string{"worker"}, Permissions: launchprofile.AgentPermissions{PermissionMode: config.PermissionModeBypass}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ManagerBypass(&config.Catalog{Global: tc.global, Agents: map[string]config.Agent{"mgr": tc.agent}})
			if !tc.warn {
				if len(got) != 0 {
					t.Fatalf("want no findings, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Status != StatusWarn || got[0].Entity != "agent:mgr" || !strings.Contains(got[0].Message, "blanket bypass") {
				t.Fatalf("want one manager-bypass warning for agent:mgr, got %+v", got)
			}
		})
	}
}
