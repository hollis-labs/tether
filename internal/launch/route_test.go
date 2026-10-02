package launch

import (
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"gopkg.in/yaml.v3"
)

func TestResolveCatalogRoute(t *testing.T) {
	var agent config.Agent
	if err := yaml.Unmarshal([]byte("id: agent\nroute:\n  channel: ops\n"), &agent); err != nil {
		t.Fatal(err)
	}
	cat := &config.Catalog{
		Agents:    map[string]config.Agent{"agent": agent},
		Projects:  map[string]config.Project{"project": {ID: "project"}},
		Providers: map[string]config.Provider{"stub": {ID: "stub"}},
		Launches:  map[string]config.Launch{"launch": {ID: "launch", Agent: "agent", Project: "project", Provider: "stub"}},
	}
	plan, err := Resolve(cat, Input{LaunchID: "launch"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Route == nil || plan.Route.Channel != "ops" || len(plan.Route.Kinds) != 4 {
		t.Fatalf("agent route=%+v", plan.Route)
	}
	l := cat.Launches["launch"]
	l.Route = &launchprofile.Route{Channel: "launch", Kinds: []string{"failure"}}
	cat.Launches["launch"] = l
	plan, err = Resolve(cat, Input{LaunchID: "launch"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Route.Channel != "launch" || len(plan.Route.Kinds) != 1 {
		t.Fatalf("launch route=%+v", plan.Route)
	}
	agent.Route = nil
	cat.Agents["agent"] = agent
	l.Route = nil
	cat.Launches["launch"] = l
	plan, err = Resolve(cat, Input{LaunchID: "launch"})
	if err != nil || plan.Route != nil {
		t.Fatalf("default route=%+v, err=%v", plan.Route, err)
	}
}
