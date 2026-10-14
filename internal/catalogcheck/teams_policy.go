package catalogcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/teamspolicy"
)

// TeamsPolicy checks teams-policy.yaml the way the daemon will load it: a file
// that fails teamspolicy.Parse (including the one-launch-per-definition-pin
// rule) takes teams down at the next restart, so it is a failure here. Launches
// the policy names are checked against the catalog: a missing or disabled
// launch or agent fails `team form` with 409, and a launch with no worker whose
// project repo_root is a protection placeholder would run on the hub and fail
// with the placeholder repo_root error, so those are warnings. An absent file
// yields no results.
func TeamsPolicy(cat *config.Catalog, root string) []Result {
	raw, err := os.ReadFile(teamspolicy.Path(root)) //nolint:gosec // operator-owned file under the catalog root
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return []Result{Fail("teams-policy", err.Error(), "make teams-policy.yaml readable")}
	}
	pol, err := teamspolicy.Parse(raw)
	if err != nil {
		return []Result{Fail("teams-policy", err.Error(), "fix teams-policy.yaml; the daemon refuses to start teams with an invalid policy")}
	}
	var out []Result
	ids := pol.LaunchIDs()
	sort.Strings(ids)
	for _, id := range ids {
		launch, ok := cat.Launches[id]
		if !ok {
			out = append(out, Warn("teams-policy-launch", fmt.Sprintf("policy launch %q is not in the catalog", id), "add the launch or remove it from teams-policy.yaml"))
			continue
		}
		if !launch.IsEnabled() {
			out = append(out, Warn("teams-policy-launch", fmt.Sprintf("policy launch %q is disabled; team form returns 409", id), "enable the launch or remove it from teams-policy.yaml"))
		}
		if agent, ok := cat.Agents[launch.Agent]; ok && !agent.IsEnabled() {
			out = append(out, Warn("teams-policy-launch", fmt.Sprintf("policy launch %q uses disabled agent %q; team form returns 409", id, launch.Agent), "enable the agent or remove the launch from teams-policy.yaml"))
		}
		if launch.Worker == "" {
			if proj, ok := cat.Projects[launch.Project]; ok && isPlaceholderRoot(proj.RepoRoot) {
				out = append(out, Warn("teams-policy-launch", fmt.Sprintf("policy launch %q has no worker and project %q repo_root %s is a placeholder, so it resolves to hub placement and team form fails", id, launch.Project, proj.RepoRoot), "set worker: on the launch or fix the project repo_root"))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, OK("teams-policy", fmt.Sprintf("teams-policy.yaml parses and its %d launches are usable", len(ids))))
	}
	return out
}

func isPlaceholderRoot(repoRoot string) bool {
	if repoRoot == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(config.Expand(repoRoot), ".tether", config.PlaceholderMarker))
	return err == nil
}
