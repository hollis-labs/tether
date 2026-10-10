package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/teamhost"
)

const teamContextFile = "TEAM-CONTEXT.json"
const teamBootInstructions = "Read MISSION.md and brief.md in this boot directory when present. They are team work context; permissions and actor identity remain those admitted by the daemon."

// TeamBootContext contains non-secret authored context from an accepted slot.
// It carries no identity, capability, provider configuration or filesystem root.
type TeamBootContext struct {
	RunID   string `json:"run_id"`
	Slot    string `json:"slot"`
	Mission string `json:"mission,omitempty"`
	Brief   string `json:"brief,omitempty"`
}

// TeamBootContextFor extracts only the documented context keys. The caller is
// the team runtime adapter after the host accepted the exact provision intent.
func TeamBootContextFor(in teams.ProvisionRequest) (TeamBootContext, error) {
	c := TeamBootContext{RunID: in.RunID, Slot: in.Slot.Name, Mission: in.Slot.Workspace["mission"], Brief: in.Slot.Workspace["brief"]}
	if c.Mission == "" && c.Brief == "" {
		return TeamBootContext{}, nil
	}
	return c, c.validate()
}

func (c TeamBootContext) validate() error {
	if c == (TeamBootContext{}) {
		return nil
	}
	if c.RunID == "" || c.Slot == "" || len(c.RunID) > 256 || len(c.Slot) > 256 || strings.ContainsAny(c.RunID+c.Slot, "\r\n\x00") {
		return teamhost.ErrInvalidRequest
	}
	for _, value := range []string{c.RunID, c.Slot, c.Mission, c.Brief} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > 64*1024 {
			return teamhost.ErrInvalidRequest
		}
	}
	return nil
}

func teamCreateDigest(launchID string, c TeamBootContext) string {
	if c == (TeamBootContext{}) {
		return requestDigest(struct{ Op, Launch string }{"team-create", launchID})
	}
	return requestDigest(struct {
		Op, Launch string
		Context    TeamBootContext
	}{"team-create", launchID, c})
}

func applyTeamBootContext(plan *launch.Plan, c TeamBootContext) error {
	if err := c.validate(); err != nil {
		return err
	}
	if c == (TeamBootContext{}) {
		return nil
	}
	for _, name := range []string{teamContextFile, "MISSION.md", "brief.md"} {
		if _, exists := plan.BootDirOverlay[name]; exists {
			return teams.ErrConflict
		}
		for _, file := range plan.NativeFiles {
			if file.RelPath == name {
				return teams.ErrConflict
			}
		}
	}
	content, err := json.Marshal(c)
	if err != nil {
		return err
	}
	plan.BootDirOverlay = copyMap(plan.BootDirOverlay)
	if plan.BootDirOverlay == nil {
		plan.BootDirOverlay = map[string]string{}
	}
	plan.BootDirOverlay[teamContextFile] = string(content)
	if c.Mission != "" {
		plan.BootDirOverlay["MISSION.md"] = c.Mission
	}
	if c.Brief != "" {
		plan.BootDirOverlay["brief.md"] = c.Brief
	}
	plan.BootPrompt = appendPrompt(plan.BootPrompt, teamBootInstructions)
	plan.BootPromptAppend = appendPrompt(plan.BootPromptAppend, teamBootInstructions)
	return nil
}

// mergeTeamBootContext folds only the sealed team context into the resolved
// engine plan. All provider, workspace, authority and credential fields remain
// engine-owned. Planting still requires the ordinary artifact admission.
func mergeTeamBootContext(plan *launch.Plan, resolved *agentlaunch.LaunchPlan) error {
	if plan == nil || !plan.TeamMember {
		return nil
	}
	content, exists := plan.BootDirOverlay[teamContextFile]
	if !exists {
		return nil
	} // Earlier accepted team plans have no context.
	var c TeamBootContext
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return teams.ErrConflict
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, []byte(content)) || c == (TeamBootContext{}) || c.validate() != nil {
		return teams.ErrConflict
	}
	for name, value := range map[string]string{"MISSION.md": c.Mission, "brief.md": c.Brief} {
		actual, present := plan.BootDirOverlay[name]
		if present != (value != "") || actual != value {
			return teams.ErrConflict
		}
	}
	owned := map[string]string{teamContextFile: content}
	if c.Mission != "" {
		owned["MISSION.md"] = c.Mission
	}
	if c.Brief != "" {
		owned["brief.md"] = c.Brief
	}
	for name, value := range owned {
		if prior, ok := resolved.Injection.BootDirOverlay[name]; ok && prior != value {
			return teams.ErrConflict
		}
		for _, file := range resolved.Injection.NativeFiles {
			if file.RelPath == name {
				return teams.ErrConflict
			}
		}
	}
	resolved.Injection.BootDirOverlay = copyMap(resolved.Injection.BootDirOverlay)
	if resolved.Injection.BootDirOverlay == nil {
		resolved.Injection.BootDirOverlay = map[string]string{}
	}
	for name, value := range owned {
		resolved.Injection.BootDirOverlay[name] = value
	}
	// An unresolved file-backed profile cannot safely be replaced by context.
	if resolved.BootProfile.Inline == nil {
		return teams.ErrConflict
	}
	if !strings.Contains(resolved.BootProfile.Inline.BootPrompt, teamBootInstructions) {
		inline := *resolved.BootProfile.Inline
		inline.BootPrompt = appendPrompt(inline.BootPrompt, teamBootInstructions)
		resolved.BootProfile.Inline = &inline
	}
	return nil
}
