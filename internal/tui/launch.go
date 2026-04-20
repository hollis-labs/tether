package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
)

// launchResultMsg carries the outcome of a CreateAndLaunch call into
// Update for toast rendering.
type launchResultMsg struct {
	req client.CreateAndLaunchRequest
	res client.CreateAndLaunchResponse
	err error
}

// sessionFetchedForAttachMsg carries a freshly-fetched session DTO that
// the main screen dispatches on provider-kind: claude-stream →
// ChatScreen; everything else → AttachScreen. Emitted by getSessionCmd
// after a successful launch so the auto-attach picks the right surface.
type sessionFetchedForAttachMsg struct {
	sess api.SessionDTO
	err  error
	// launchReq is preserved so the fallback path (fetch error) can
	// still show a meaningful toast.
	launchReq client.CreateAndLaunchRequest
	launchRes client.CreateAndLaunchResponse
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

// getSessionForAttachCmd fetches a SessionDTO by ID so the main screen
// can pick the right detail screen to auto-attach into. Takes the
// launch request/response so the resulting message carries the toast
// context if the fetch fails.
func getSessionForAttachCmd(c *client.Client, req client.CreateAndLaunchRequest, res client.CreateAndLaunchResponse) tea.Cmd {
	return func() tea.Msg {
		sess, err := c.GetSession(context.Background(), res.SessionID)
		return sessionFetchedForAttachMsg{sess: sess, err: err, launchReq: req, launchRes: res}
	}
}
