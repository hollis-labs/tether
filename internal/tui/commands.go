package tui

import (
	"bytes"
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/bootgen"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/externshell"
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

// loadAllCatalogCmd returns a tea.Batch of List* commands that run in parallel.
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
		// Build lookup maps: launchID → providerID, providerID → command.
		// These let the row display the harness name and let bootDirectCmd
		// know which binary to invoke without an extra daemon round-trip.
		providerByLaunch := map[string]string{}
		commandByProvider := map[string]string{}
		if launches, lerr := c.ListLaunches(context.Background()); lerr == nil {
			for _, l := range launches {
				providerByLaunch[l.ID] = l.Provider
			}
		}
		if providers, perr := c.ListProviders(context.Background()); perr == nil {
			for _, p := range providers {
				commandByProvider[p.ID] = p.Command
			}
		}
		rows := make([]BootProfileRow, 0, len(profiles))
		for _, p := range profiles {
			pid := providerByLaunch[p.Launch]
			rows = append(rows, BootProfileRow{
				ProfileID:       p.ID,
				DisplayName:     p.DisplayName,
				LaunchID:        p.Launch,
				ProviderID:      pid,
				ProviderCommand: commandByProvider[pid],
				Profile:         p,
			})
		}
		return catalogLoadedMsg{typ: RowTypeBootProfiles, rows: rowsFromBootProfiles(rows)}
	}
}

// bootDirectMsg carries the outcome of a bootDirectCmd run.
// This is the boot-profile quicklaunch path — no mux session is created.
type bootDirectMsg struct {
	profileID string
	err       error
}

// bootDirectCmd is the boot-profile quicklaunch action. It:
//  1. Generates the boot prompt from the profile (local, no daemon)
//  2. Opens iTerm2/Terminal.app running the tool directly with the prompt
//
// No mux session is created. The tool (claude, opencode, …) runs natively
// in the user's terminal with the generated context as its first input.
// Equivalent to: mux generate-boot <profile> | claude --dangerously-skip-permissions
func bootDirectCmd(c *client.Client, row BootProfileRow) tea.Cmd {
	return func() tea.Msg {
		if row.LaunchID == "" {
			return bootDirectMsg{profileID: row.ProfileID,
				err: fmt.Errorf("profile %q has no launch configured", row.ProfileID)}
		}
		if row.ProviderCommand == "" {
			return bootDirectMsg{profileID: row.ProfileID,
				err: fmt.Errorf("no provider command for %q — check catalog", row.ProfileID)}
		}
		var buf bytes.Buffer
		if err := bootgen.Generate(context.Background(), row.Profile, c.CatalogRoot, &buf); err != nil {
			return bootDirectMsg{profileID: row.ProfileID, err: fmt.Errorf("generate boot prompt: %w", err)}
		}
		workDir := config.Expand(row.Profile.Identity.WorkRoot)
		if workDir == "" {
			workDir = "."
		}
		if err := externshell.BootWith(buf.String(), row.ProviderID, row.ProviderCommand, nil, workDir); err != nil {
			return bootDirectMsg{profileID: row.ProfileID, err: fmt.Errorf("open terminal: %w", err)}
		}
		return bootDirectMsg{profileID: row.ProfileID}
	}
}

// bootLaunchDirectCmd is the interactive-provider quicklaunch action for
// LaunchRows. It loads the catalog from disk, runs launch.Resolve to get the
// full boot prompt (project/agent fragments, knowledge base, provider prefix)
// along with the resolved command, args, and workDir, then opens an external
// terminal via externshell.BootWith.
//
// Using launch.Resolve mirrors the daemon's boot path so the generated prompt
// is identical to what a daemon-managed session would receive.
// The daemon path (launchCmd) is kept for non-interactive providers.
func bootLaunchDirectCmd(c *client.Client, row LaunchRow) tea.Cmd {
	return func() tea.Msg {
		if c.CatalogRoot == "" {
			return bootDirectMsg{profileID: row.L.ID, err: fmt.Errorf("catalog root not set")}
		}

		cat, err := config.Load(c.CatalogRoot)
		if err != nil {
			return bootDirectMsg{profileID: row.L.ID, err: fmt.Errorf("load catalog: %w", err)}
		}

		plan, err := launch.Resolve(cat, launch.Input{
			LaunchID:    row.L.ID,
			CatalogRoot: c.CatalogRoot,
		})
		if err != nil {
			return bootDirectMsg{profileID: row.L.ID, err: fmt.Errorf("resolve launch: %w", err)}
		}

		if plan.Command == "" {
			return bootDirectMsg{profileID: row.L.ID,
				err: fmt.Errorf("no command for provider %q — check catalog", plan.ProviderID)}
		}

		workDir := plan.RepoRoot
		if workDir == "" {
			workDir = "."
		}

		if err := externshell.BootWith(plan.BootPrompt, plan.ProviderID, plan.Command, plan.Args, workDir); err != nil {
			return bootDirectMsg{profileID: row.L.ID, err: fmt.Errorf("open terminal: %w", err)}
		}
		return bootDirectMsg{profileID: row.L.ID}
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
