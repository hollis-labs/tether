package mcpadapter

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/app"
	"github.com/chrispian/agent-mux/internal/runtime"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/store"
)

func (a *Adapter) registerSessionTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("mux_session_list",
		mcp.WithDescription("List agent sessions. Optionally filter by state (created, running, stopped, failed) and paginate with cursor and limit."),
		mcp.WithString("state", mcp.Description("Filter by session state: created, running, stopped, failed")),
		mcp.WithString("cursor", mcp.Description("RFC3339 pagination cursor — returns sessions older than this timestamp")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 50, max 200)")),
	), a.handleSessionList)

	s.AddTool(mcp.NewTool("mux_session_get",
		mcp.WithDescription("Get a single agent session by ID."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
	), a.handleSessionGet)

	s.AddTool(mcp.NewTool("mux_session_create",
		mcp.WithDescription("Create a session from a launch profile (state=created, not yet running). Follow with mux_session_launch to start it. Optionally inject a boot prompt override."),
		mcp.WithString("launch_id", mcp.Required(), mcp.Description("Launch profile ID from the catalog (see mux_catalog_list_launches)")),
		mcp.WithString("boot_prompt", mcp.Description("Optional boot prompt override; replaces catalog static boot fragments")),
	), a.handleSessionCreate)

	s.AddTool(mcp.NewTool("mux_session_launch",
		mcp.WithDescription("Start a previously created session (transitions from created → running). Returns launch details including workspace path and log path."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID returned by mux_session_create")),
	), a.handleSessionLaunch)

	s.AddTool(mcp.NewTool("mux_session_stop",
		mcp.WithDescription("Send a stop signal to a running session."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
	), a.handleSessionStop)

	s.AddTool(mcp.NewTool("mux_session_wait",
		mcp.WithDescription("Block until the session exits and return its exit code. Use after mux_session_stop or for short-lived sessions."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
	), a.handleSessionWait)

	s.AddTool(mcp.NewTool("mux_session_send_input",
		mcp.WithDescription("Send raw text input to a running session's stdin (PTY). Use to interact with a CLI agent session."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
		mcp.WithString("input", mcp.Required(), mcp.Description("Text to send to the session (a newline is NOT appended automatically)")),
	), a.handleSessionSendInput)

	s.AddTool(mcp.NewTool("mux_session_resize",
		mcp.WithDescription("Resize the PTY terminal for a running session."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
		mcp.WithNumber("rows", mcp.Required(), mcp.Description("Terminal rows (must be > 0)")),
		mcp.WithNumber("cols", mcp.Required(), mcp.Description("Terminal columns (must be > 0)")),
	), a.handleSessionResize)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleSessionList(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := intArg(req, "limit", 50)
	if limit > 200 {
		limit = 200
	}
	opts := store.ListSessionsOptions{
		State:  str(req, "state"),
		Cursor: str(req, "cursor"),
		Limit:  limit,
	}
	rows, err := a.svc.ListSessions(opts)
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}
	dtos := make([]api.SessionDTO, 0, len(rows))
	for _, r := range rows {
		dto := api.SessionRowToDTO(r)
		dto.AttachedClients = a.svc.AttachedClients(r.ID)
		dtos = append(dtos, dto)
	}
	out := map[string]any{
		"ok":       true,
		"sessions": dtos,
		"count":    len(dtos),
	}
	if len(rows) == limit && len(rows) > 0 {
		out["next_cursor"] = rows[len(rows)-1].CreatedAt
	}
	return toolJSON(out), nil
}

func (a *Adapter) handleSessionGet(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	row, err := a.svc.GetSession(id)
	if err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not found: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	dto := api.SessionRowToDTO(*row)
	dto.AttachedClients = a.svc.AttachedClients(id)
	return toolJSON(map[string]any{"ok": true, "session": dto}), nil
}

func (a *Adapter) handleSessionCreate(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	launchID := str(req, "launch_id")
	if launchID == "" {
		return toolError("invalid_request", "launch_id required"), nil
	}
	bootPrompt := str(req, "boot_prompt")

	var res *app.Launched
	var err error
	if bootPrompt != "" {
		res, err = a.svc.CreateSessionWithBootPrompt(launchID, bootPrompt)
	} else {
		res, err = a.svc.CreateSession(launchID)
	}
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}
	wsPath := ""
	logPath := ""
	if res.Workspace != nil {
		wsPath = res.Workspace.Root
		logPath = res.Workspace.LogPath
	}
	return toolJSON(map[string]any{
		"ok":               true,
		"session_id":       res.SessionID,
		"workspace":        wsPath,
		"log":              logPath,
		"provider_id":      res.Plan.ProviderID,
		"provider_kind":    res.ProviderKind,
		"logical_agent_id": res.Plan.LogicalAgentID,
	}), nil
}

func (a *Adapter) handleSessionLaunch(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	res, err := a.svc.LaunchSession(id)
	if err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not found: "+id), nil
		}
		if isConflict(err) {
			return toolError("conflict", err.Error()), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	wsPath := ""
	logPath := ""
	if res.Workspace != nil {
		wsPath = res.Workspace.Root
		logPath = res.Workspace.LogPath
	}
	return toolJSON(map[string]any{
		"ok":               true,
		"session_id":       res.SessionID,
		"workspace":        wsPath,
		"log":              logPath,
		"provider_id":      res.Plan.ProviderID,
		"provider_kind":    res.ProviderKind,
		"logical_agent_id": res.Plan.LogicalAgentID,
	}), nil
}

func (a *Adapter) handleSessionStop(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	if err := a.svc.StopSession(id); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not running: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id}), nil
}

func (a *Adapter) handleSessionWait(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	code, err := a.svc.WaitSession(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not running: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "exit_code": code}), nil
}

func (a *Adapter) handleSessionSendInput(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	input := str(req, "input")
	if input == "" {
		return toolError("invalid_request", "input required"), nil
	}
	if err := a.svc.SendInput(id, []byte(input)); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not found: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(input)}), nil
}

func (a *Adapter) handleSessionResize(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	rowsInt := intArg(req, "rows", 0)
	colsInt := intArg(req, "cols", 0)
	if rowsInt <= 0 || colsInt <= 0 || rowsInt > 65535 || colsInt > 65535 {
		return toolError("invalid_request", "rows and cols must be between 1 and 65535"), nil
	}
	rows := uint16(rowsInt) //nolint:gosec // range validated above
	cols := uint16(colsInt) //nolint:gosec // range validated above
	if err := a.svc.ResizeSession(id, rows, cols); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not running: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "rows": rows, "cols": cols}), nil
}

// ─── error helpers ─────────────────────────────────────────────────────────────

// isNotFound reports whether err is a "not found" class error. Uses
// errors.Is against sentinel values (ADR 0022 G6) to avoid fragile
// string matching. Covers session, runtime, and messaging not-found sentinels.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, store.ErrSessionNotFound) ||
		errors.Is(err, runtime.ErrSessionNotRunning) ||
		errors.Is(err, messaging.ErrNotFound)
}

// isWrongRecipient reports whether err signals that the caller is not
// the intended recipient of a message (store.ErrWrongRecipient).
// Used by handleMessageConsume to return a distinct conflict error.
func isWrongRecipient(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, store.ErrWrongRecipient)
}

// isConflict reports whether err is a "conflict" class error — specifically
// that a session lifecycle precondition failed (e.g. launching a session
// that is not in the created state). Uses errors.Is against session.ErrNotCreated.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, session.ErrNotCreated)
}
