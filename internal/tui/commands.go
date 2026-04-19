package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/client"
)

// catalogLoadedMsg carries the outcome of a single List* call into
// Update. When err is non-nil the rows slice will be empty; Update
// stores the error for display in the footer.
type catalogLoadedMsg struct {
	typ  RowType
	rows []ResultRow
	err  error
}

// loadAllCatalogCmd returns a tea.Batch of five List* commands that
// run in parallel. Each emits a catalogLoadedMsg with its own RowType.
func loadAllCatalogCmd(c *client.Client) tea.Cmd {
	return tea.Batch(
		loadProjectsCmd(c),
		loadAgentsCmd(c),
		loadProvidersCmd(c),
		loadLaunchesCmd(c),
		loadSessionsCmd(c),
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
