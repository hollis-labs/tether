package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/client"
)

// launchResultMsg carries the outcome of a CreateAndLaunch call into
// Update for toast rendering.
type launchResultMsg struct {
	req client.CreateAndLaunchRequest
	res client.CreateAndLaunchResponse
	err error
}

// launchCmd wraps client.CreateAndLaunch in a tea.Cmd so the daemon
// round-trip runs on Bubble Tea's worker goroutine, not the Update
// thread.
func launchCmd(c *client.Client, req client.CreateAndLaunchRequest) tea.Cmd {
	return func() tea.Msg {
		res, err := c.CreateAndLaunch(context.Background(), req)
		return launchResultMsg{req: req, res: res, err: err}
	}
}
