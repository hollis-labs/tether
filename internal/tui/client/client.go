// Package client wraps internal/client for Bubble Tea consumption.
//
// The wrapper exists for three reasons:
//
//  1. A TUI-stable surface. As the TUI grows, it can evolve typed
//     request/response shapes (e.g., CreateAndLaunchRequest) without
//     bloating the shared daemon client.
//  2. Uniform error wrapping. Every error carries a "tui client:"
//     prefix so a tea.Cmd-returned message reads clearly without the
//     caller having to prefix each failure themselves.
//  3. Documentation seam. This is the home of the "never block Update"
//     contract documented in the package README.
//
// ## Usage contract — never block Update
//
// Bubble Tea's Update runs on a single goroutine. Calling a List* or
// CreateAndLaunch method from inside Update blocks all input handling
// until the daemon responds. Always wrap these calls in a tea.Cmd:
//
//	func listProjectsCmd(c *client.Client) tea.Cmd {
//	    return func() tea.Msg {
//	        ps, err := c.ListProjects(context.Background())
//	        if err != nil {
//	            return ProjectsLoadErrMsg{Err: err}
//	        }
//	        return ProjectsLoadedMsg{Projects: ps}
//	    }
//	}
//
// Bubble Tea runs the returned function on its own goroutine and pipes
// the result back into Update as a tea.Msg. That keeps input latency
// flat even on a slow daemon.
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	daemon "github.com/chrispian/agent-mux/internal/client"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/bootgen"
	"github.com/chrispian/agent-mux/internal/config"
)

// ListBootProfiles loads boot profiles from the catalog root.
// Returns an empty list when CatalogRoot is not set or the directory is missing.
func (c *Client) ListBootProfiles() ([]bootgen.Profile, error) {
	if c.CatalogRoot == "" {
		return nil, nil
	}
	dir := c.CatalogRoot + "/boot-profiles"
	profiles, err := bootgen.LoadProfiles(dir)
	if err != nil {
		return nil, fmt.Errorf("tui client: list boot profiles: %w", err)
	}
	out := make([]bootgen.Profile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p)
	}
	return out, nil
}

// BootAndLaunch generates the boot prompt for profileID and launches a
// session using the profile's configured launch ID.
func (c *Client) BootAndLaunch(ctx context.Context, profileID string) (CreateAndLaunchResponse, error) {
	if c.CatalogRoot == "" {
		return CreateAndLaunchResponse{}, fmt.Errorf("tui client: catalog root not set")
	}
	dir := c.CatalogRoot + "/boot-profiles"
	profiles, err := bootgen.LoadProfiles(dir)
	if err != nil {
		return CreateAndLaunchResponse{}, fmt.Errorf("tui client: load profiles: %w", err)
	}
	p, ok := profiles[profileID]
	if !ok {
		return CreateAndLaunchResponse{}, fmt.Errorf("tui client: boot profile %q not found", profileID)
	}
	if p.Launch == "" {
		return CreateAndLaunchResponse{}, fmt.Errorf("tui client: profile %q has no launch configured", profileID)
	}
	var buf bytes.Buffer
	if err := bootgen.Generate(ctx, p, c.CatalogRoot, &buf); err != nil {
		return CreateAndLaunchResponse{}, fmt.Errorf("tui client: generate boot: %w", err)
	}
	return c.CreateAndLaunchWithBootPrompt(ctx, p.Launch, buf.String())
}

// CreateAndLaunchWithBootPrompt creates a session with a dynamically generated
// boot prompt, launches it, and returns the result for auto-attach.
func (c *Client) CreateAndLaunchWithBootPrompt(ctx context.Context, launchID, bootPrompt string) (CreateAndLaunchResponse, error) {
	created, err := c.inner.CreateSessionWithBootPrompt(ctx, launchID, bootPrompt)
	if err != nil {
		return CreateAndLaunchResponse{}, wrap("create session with boot prompt", err)
	}
	launched, err := c.inner.LaunchSession(ctx, created.ID)
	if err != nil {
		return CreateAndLaunchResponse{}, wrap("launch session", err)
	}
	return CreateAndLaunchResponse{SessionID: launched.ID}, nil
}

// ErrDaemonUnreachable is re-exported from the underlying client so
// TUI callers don't need to import internal/client just to check for
// the sentinel. Use errors.Is, not ==.
var ErrDaemonUnreachable = daemon.ErrDaemonUnreachable

// Client is the TUI's handle for the muxd daemon local API. Safe for
// concurrent use from multiple tea.Cmd goroutines.
type Client struct {
	inner       *daemon.Client
	CatalogRoot string // set by NewWithCatalog; enables in-process boot profile loading
}

// New constructs a Client pointing at the resolved daemon listen
// address (e.g. "unix:/Users/you/.agent-mux/run/muxd.sock" or
// "tcp:127.0.0.1:9000").
func New(listenAddr string) *Client {
	return &Client{inner: daemon.New(listenAddr)}
}

// NewWithCatalog constructs a Client that also knows the catalog root,
// enabling in-process operations like boot profile loading.
func NewWithCatalog(listenAddr, catalogRoot string) *Client {
	return &Client{inner: daemon.New(listenAddr), CatalogRoot: catalogRoot}
}

// Ping issues a short /health probe so the TUI can surface a
// daemon-down banner at startup without loading any catalog data.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.inner.Ping(ctx); err != nil {
		return wrap("ping", err)
	}
	return nil
}

// ListProjects fetches project definitions from GET /catalog/projects.
func (c *Client) ListProjects(ctx context.Context) ([]config.Project, error) {
	ps, err := c.inner.ListProjects(ctx)
	if err != nil {
		return nil, wrap("list projects", err)
	}
	return ps, nil
}

// ListAgents fetches agent definitions from GET /catalog/agents.
func (c *Client) ListAgents(ctx context.Context) ([]config.Agent, error) {
	as, err := c.inner.ListAgents(ctx)
	if err != nil {
		return nil, wrap("list agents", err)
	}
	return as, nil
}

// ListProviders fetches provider definitions from GET /catalog/providers.
func (c *Client) ListProviders(ctx context.Context) ([]config.Provider, error) {
	ps, err := c.inner.ListProviders(ctx)
	if err != nil {
		return nil, wrap("list providers", err)
	}
	return ps, nil
}

// ListLaunches fetches launch profiles from GET /catalog/launches.
func (c *Client) ListLaunches(ctx context.Context) ([]config.Launch, error) {
	ls, err := c.inner.ListLaunches(ctx)
	if err != nil {
		return nil, wrap("list launches", err)
	}
	return ls, nil
}

// ListSessions fetches the current session page from GET /sessions.
// The TUI displays a single unpaginated page in Sprint 1; Sprint 2's
// detail views add cursor-paging where needed.
func (c *Client) ListSessions(ctx context.Context) ([]api.SessionDTO, error) {
	res, err := c.inner.ListSessions(ctx, daemon.ListOptions{})
	if err != nil {
		return nil, wrap("list sessions", err)
	}
	return res.Sessions, nil
}

// GetSession fetches a single session DTO by ID via GET /sessions/{id}.
// Used by the main screen's launch-and-attach flow to determine the
// provider kind (claude-stream vs claude-code etc.) before choosing
// which detail screen to push.
func (c *Client) GetSession(ctx context.Context, sessionID string) (api.SessionDTO, error) {
	dto, err := c.inner.GetSession(ctx, sessionID)
	if err != nil {
		return api.SessionDTO{}, wrap("get session", err)
	}
	return dto, nil
}

// CreateAndLaunchRequest drives the quick-launch flow triggered by
// Enter on a LaunchRow. Only LaunchID is populated for v0.0.3 Sprint 1;
// Sprint 4's wizard extends this shape with inline-plan fields.
type CreateAndLaunchRequest struct {
	LaunchID string
}

// CreateAndLaunchResponse carries what the TUI surfaces in the
// launch-result toast: session ID, workspace path, and log path.
type CreateAndLaunchResponse struct {
	SessionID string
	Workspace string
	LogPath   string
}

// CreateAndLaunch performs the CLI's convenience create+launch pair in
// one call. The daemon exposes the two operations separately; the
// combined flow lives on the client to keep UI call-sites concise.
func (c *Client) CreateAndLaunch(ctx context.Context, req CreateAndLaunchRequest) (CreateAndLaunchResponse, error) {
	if req.LaunchID == "" {
		return CreateAndLaunchResponse{}, errors.New("tui client: create+launch: launch_id is required")
	}
	res, err := c.inner.Launch(ctx, req.LaunchID)
	if err != nil {
		return CreateAndLaunchResponse{}, wrap("create+launch", err)
	}
	return CreateAndLaunchResponse{
		SessionID: res.ID,
		Workspace: res.Workspace,
		LogPath:   res.Log,
	}, nil
}

// StopSession asks the daemon to terminate a running session. Returns
// nil on success; an error for unreachable-daemon or non-2xx response.
func (c *Client) StopSession(ctx context.Context, sessionID string) error {
	if err := c.inner.StopSession(ctx, sessionID); err != nil {
		return wrap("stop session", err)
	}
	return nil
}

// ResizeSession forwards a (rows, cols) winsize update to the named
// session's PTY. Primary caller is the attach screen on every
// tea.WindowSizeMsg while attached.
func (c *Client) ResizeSession(ctx context.Context, sessionID string, rows, cols uint16) error {
	if err := c.inner.ResizeSession(ctx, sessionID, rows, cols); err != nil {
		return wrap("resize", err)
	}
	return nil
}

// AttachStream opens a long-running byte stream from the daemon's
// /sessions/{id}/attach endpoint. Bytes arrive on w in the order the
// session emits them. Returns when ctx is canceled (caller detaches)
// or the session exits.
//
// Per ADR 0011 attach is plain application/octet-stream — no SSE, no
// JSON envelope, no base64. Callers write straight PTY bytes into w.
func (c *Client) AttachStream(ctx context.Context, sessionID string, w io.Writer) error {
	if err := c.inner.AttachSession(ctx, sessionID, w, 0); err != nil {
		return wrap("attach", err)
	}
	return nil
}

// SendInput forwards raw bytes to the session's stdin via
// POST /sessions/{id}/input. The daemon treats the body as opaque
// octet-stream — no framing, no newline handling.
func (c *Client) SendInput(ctx context.Context, sessionID string, data []byte) error {
	if err := c.inner.SendInput(ctx, sessionID, data); err != nil {
		return wrap("send input", err)
	}
	return nil
}

// CreateCheckpoint posts a checkpoint for the given session. Summary is
// the free-text content; status is one of active/paused/completed/escalated.
func (c *Client) CreateCheckpoint(ctx context.Context, sessionID, status, summary string) error {
	if err := c.inner.CreateCheckpoint(ctx, sessionID, status, summary); err != nil {
		return wrap("create checkpoint", err)
	}
	return nil
}

// ResumeLogicalAgent posts a resume request for the given logical agent
// and returns the new session ID.
func (c *Client) ResumeLogicalAgent(ctx context.Context, agentID string) (string, error) {
	id, err := c.inner.ResumeLogicalAgent(ctx, agentID)
	if err != nil {
		return "", wrap("resume", err)
	}
	return id, nil
}

// ListLogicalAgents fetches all logical agents from the daemon.
func (c *Client) ListLogicalAgents(ctx context.Context) ([]api.LogicalAgentSummary, error) {
	rows, err := c.inner.ListLogicalAgents(ctx)
	if err != nil {
		return nil, wrap("list logical agents", err)
	}
	return rows, nil
}

// wrap prefixes "tui client: <op>" while preserving the sentinel
// (errors.Is(err, ErrDaemonUnreachable) still works in callers).
func wrap(op string, err error) error {
	return fmt.Errorf("tui client: %s: %w", op, err)
}
