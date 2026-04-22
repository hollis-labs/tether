package tui

import (
	"fmt"
	"strings"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/config"
)

// ResultRow is the uniform shape the results viewport renders. Each
// concrete row type wraps its source record so T-05 and later sprints
// can downcast (via Record) when they need typed access — e.g., Enter
// on a LaunchRow needs the launch ID.
type ResultRow interface {
	Type() RowType
	ID() string
	Title() string
	Subtitle() string
	Record() any
}

// ProjectRow wraps config.Project.
type ProjectRow struct{ P config.Project }

func (r ProjectRow) Type() RowType { return RowTypeProjects }
func (r ProjectRow) ID() string    { return r.P.ID }
func (r ProjectRow) Title() string { return displayName(r.P.Name, r.P.ID) }
func (r ProjectRow) Subtitle() string {
	return "project  ·  " + trimPath(r.P.RepoRoot)
}
func (r ProjectRow) Record() any { return r.P }

// AgentRow wraps config.Agent.
type AgentRow struct{ A config.Agent }

func (r AgentRow) Type() RowType { return RowTypeAgents }
func (r AgentRow) ID() string    { return r.A.ID }
func (r AgentRow) Title() string { return displayName(r.A.Name, r.A.ID) }
func (r AgentRow) Subtitle() string {
	roles := strings.Join(r.A.Roles, ", ")
	if roles == "" {
		roles = "—"
	}
	return "agent    ·  roles: " + roles
}
func (r AgentRow) Record() any { return r.A }

// ProviderRow wraps config.Provider.
type ProviderRow struct{ P config.Provider }

func (r ProviderRow) Type() RowType { return RowTypeProviders }
func (r ProviderRow) ID() string    { return r.P.ID }
func (r ProviderRow) Title() string { return r.P.ID }
func (r ProviderRow) Subtitle() string {
	return "provider ·  type: " + r.P.Type
}
func (r ProviderRow) Record() any { return r.P }

// LaunchRow wraps config.Launch. Enter on this row type drives the
// quick-launch flow in T-05.
type LaunchRow struct{ L config.Launch }

func (r LaunchRow) Type() RowType { return RowTypeLaunches }
func (r LaunchRow) ID() string    { return r.L.ID }
func (r LaunchRow) Title() string { return r.L.ID }
func (r LaunchRow) Subtitle() string {
	return fmt.Sprintf("launch   ·  %s / %s via %s", r.L.Project, r.L.Agent, r.L.Provider)
}
func (r LaunchRow) Record() any { return r.L }

// SessionRow wraps api.SessionDTO.
type SessionRow struct{ S api.SessionDTO }

func (r SessionRow) Type() RowType { return RowTypeSessions }
func (r SessionRow) ID() string    { return r.S.ID }
func (r SessionRow) Title() string { return shortID(r.S.ID) }
func (r SessionRow) Subtitle() string {
	return fmt.Sprintf("session  ·  %s  ·  %s / %s", r.S.State, r.S.ProjectID, r.S.LogicalAgentID)
}
func (r SessionRow) Record() any { return r.S }

// BootProfileRow wraps a boot profile. Enter generates the boot prompt
// and launches a session using the profile's configured launch ID.
type BootProfileRow struct {
	ProfileID   string
	DisplayName string
	LaunchID    string // catalog launch ID configured in the profile
}

func (r BootProfileRow) Type() RowType { return RowTypeBootProfiles }
func (r BootProfileRow) ID() string    { return r.ProfileID }
func (r BootProfileRow) Title() string { return displayName(r.DisplayName, r.ProfileID) }
func (r BootProfileRow) Subtitle() string {
	if r.LaunchID != "" {
		return "boot     ·  launch: " + r.LaunchID
	}
	return "boot     ·  (no launch configured — stdout only)"
}
func (r BootProfileRow) Record() any { return r }

// LogicalAgentRow wraps api.LogicalAgentSummary. Enter on this row type
// drives the resume flow: POST /logical-agents/{id}/resume.
type LogicalAgentRow struct{ LA api.LogicalAgentSummary }

func (r LogicalAgentRow) Type() RowType { return RowTypeLogicalAgents }
func (r LogicalAgentRow) ID() string    { return r.LA.ID }
func (r LogicalAgentRow) Title() string { return displayName(r.LA.Name, r.LA.ID) }
func (r LogicalAgentRow) Subtitle() string {
	if r.LA.LaunchID != "" {
		return "runtime  ·  launch: " + r.LA.LaunchID + "  (resume available)"
	}
	return "runtime  ·  no prior launch"
}
func (r LogicalAgentRow) Record() any { return r.LA }

// rowsFromProjects / rowsFromAgents / ... adapt the concrete slice
// returned by the daemon into a []ResultRow the model can render.

func rowsFromProjects(ps []config.Project) []ResultRow {
	out := make([]ResultRow, len(ps))
	for i, p := range ps {
		out[i] = ProjectRow{P: p}
	}
	return out
}

func rowsFromAgents(as []config.Agent) []ResultRow {
	out := make([]ResultRow, len(as))
	for i, a := range as {
		out[i] = AgentRow{A: a}
	}
	return out
}

func rowsFromProviders(ps []config.Provider) []ResultRow {
	out := make([]ResultRow, len(ps))
	for i, p := range ps {
		out[i] = ProviderRow{P: p}
	}
	return out
}

func rowsFromLaunches(ls []config.Launch) []ResultRow {
	out := make([]ResultRow, len(ls))
	for i, l := range ls {
		out[i] = LaunchRow{L: l}
	}
	return out
}

func rowsFromSessions(ss []api.SessionDTO) []ResultRow {
	out := make([]ResultRow, len(ss))
	for i, s := range ss {
		out[i] = SessionRow{S: s}
	}
	return out
}

func rowsFromBootProfiles(profiles []BootProfileRow) []ResultRow {
	out := make([]ResultRow, len(profiles))
	for i, p := range profiles {
		out[i] = p
	}
	return out
}

func rowsFromLogicalAgents(las []api.LogicalAgentSummary) []ResultRow {
	out := make([]ResultRow, len(las))
	for i, la := range las {
		out[i] = LogicalAgentRow{LA: la}
	}
	return out
}

func displayName(name, id string) string {
	if name != "" {
		return name + "  (" + id + ")"
	}
	return id
}

// trimPath strips the user's home dir from a path for compact display.
// Best-effort — if $HOME is unset or not a prefix, returns as-is.
func trimPath(p string) string {
	if p == "" {
		return "—"
	}
	// Cheap HOME strip: any path starting with the HOME value is shortened
	// with ~. Keeps row heights predictable.
	if home := homeDir(); home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// shortID truncates a UUID to the first 8 characters for compact row
// display. Full IDs remain accessible via Record().
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
