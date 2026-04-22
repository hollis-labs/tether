package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/client"
)

// resumeResultMsg carries the outcome of a ResumeLogicalAgent call.
type resumeResultMsg struct {
	agentID   string
	sessionID string
	err       error
}

// catalogLoadedMsg carries the outcome of a single List* call into
// Update. When err is non-nil the rows slice will be empty; Update
// stores the error for display in the footer.
type catalogLoadedMsg struct {
	typ  RowType
	rows []ResultRow
	err  error
}

// loadAllCatalogCmd returns a tea.Batch of six List* commands that
// run in parallel. Each emits a catalogLoadedMsg with its own RowType.
func loadAllCatalogCmd(c *client.Client) tea.Cmd {
	return tea.Batch(
		loadProjectsCmd(c),
		loadAgentsCmd(c),
		loadProvidersCmd(c),
		loadLaunchesCmd(c),
		loadBootProfilesCmd(c),
		loadSessionsCmd(c),
		loadLogicalAgentsCmd(c),
	)
}

func loadProjectsCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ps, err := c.ListProjects(context.Background())
		if err != nil {
			return catalogLoadedMsg{typ: RowTypeProjects, err: err}
		}
		return catalogLoadedMsg{typ: RowTypeProjects, rows: rowsFromProjects(ps)}
	}
}

func loadAgentsCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		as, err := c.ListAgents(context.Background())
		if err != nil {
			return catalogLoadedMsg{typ: RowTypeAgents, err: err}
		}
		return catalogLoadedMsg{typ: RowTypeAgents, rows: rowsFromAgents(as)}
	}
}

func loadProvidersCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ps, err := c.ListProviders(context.Background())
		if err != nil {
			return catalogLoadedMsg{typ: RowTypeProviders, err: err}
		}
		return catalogLoadedMsg{typ: RowTypeProviders, rows: rowsFromProviders(ps)}
	}
}

func loadLaunchesCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ls, err := c.ListLaunches(context.Background())
		if err != nil {
			return catalogLoadedMsg{typ: RowTypeLaunches, err: err}
		}
		return catalogLoadedMsg{typ: RowTypeLaunches, rows: rowsFromLaunches(ls)}
	}
}

func loadSessionsCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ss, err := c.ListSessions(context.Background())
		if err != nil {
			return catalogLoadedMsg{typ: RowTypeSessions, err: err}
		}
		return catalogLoadedMsg{typ: RowTypeSessions, rows: rowsFromSessions(ss)}
	}
}

func loadBootProfilesCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		profiles, err := c.ListBootProfiles()
		if err != nil {
			return catalogLoadedMsg{typ: RowTypeBootProfiles, err: err}
		}
		rows := make([]BootProfileRow, 0, len(profiles))
		for _, p := range profiles {
			rows = append(rows, BootProfileRow{
				ProfileID:   p.ID,
				DisplayName: p.DisplayName,
				LaunchID:    p.Launch,
			})
		}
		return catalogLoadedMsg{typ: RowTypeBootProfiles, rows: rowsFromBootProfiles(rows)}
	}
}

// bootResultMsg carries the outcome of a BootAndLaunch call.
type bootResultMsg struct {
	profileID string
	sessionID string
	err       error
}

func bootAndLaunchCmd(c *client.Client, profileID string) tea.Cmd {
	return func() tea.Msg {
		res, err := c.BootAndLaunch(context.Background(), profileID)
		return bootResultMsg{profileID: profileID, sessionID: res.SessionID, err: err}
	}
}

func loadLogicalAgentsCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		las, err := c.ListLogicalAgents(context.Background())
		if err != nil {
			return catalogLoadedMsg{typ: RowTypeLogicalAgents, err: err}
		}
		return catalogLoadedMsg{typ: RowTypeLogicalAgents, rows: rowsFromLogicalAgents(las)}
	}
}

func resumeLogicalAgentCmd(c *client.Client, agentID string) tea.Cmd {
	return func() tea.Msg {
		sessID, err := c.ResumeLogicalAgent(context.Background(), agentID)
		return resumeResultMsg{agentID: agentID, sessionID: sessID, err: err}
	}
}
