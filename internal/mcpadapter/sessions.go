package mcpadapter

import (
	"context"
	"errors"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func (a *Adapter) registerSessionTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_session_list",
		mcp.WithDescription("List agent sessions. Optionally filter by state (created, running, stopped, failed) and paginate with cursor and limit."),
		mcp.WithString("state", mcp.Description("Filter by session state: created, running, stopped, failed")),
		mcp.WithString("cursor", mcp.Description("RFC3339 pagination cursor — returns sessions older than this timestamp")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 50, max 200)")),
	), Reads("GET /sessions; svc.ListSessions + AttachedClients"), a.handleSessionList)

	a.addTool(s, mcp.NewTool("mux_session_get",
		mcp.WithDescription("Get a single agent session by ID."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
	), Reads("GET /sessions/{id}; svc.GetSession + AttachedClients"), a.handleSessionGet)

	a.addTool(s, mcp.NewTool("mux_session_create",
		mcp.WithDescription("Create a session from a launch profile (state=created, not yet running). Follow with mux_session_launch to start it. Supports v005-08 Agent Ops Tier-2 caller-provided payloads (agent_file / agent_inline / boot_profile / override / prompt_append) — when any are set, they merge over the catalog-resolved agent + boot profile."),
		mcp.WithString("launch_id", mcp.Required(), mcp.Description("Launch profile ID from the catalog (see mux_catalog_list_launches)")),
		mcp.WithString("boot_prompt", mcp.Description("Optional boot prompt override; replaces catalog static boot fragments verbatim")),
		mcp.WithString("agent_file", mcp.Description("v005-08: filesystem path to an agent YAML matching config.Agent shape. Field-merged over the catalog agent.")),
		mcp.WithString("agent_inline", mcp.Description("v005-08: JSON-encoded agent definition (same shape as config.Agent). Highest precedence in agent resolve order.")),
		mcp.WithString("boot_profile", mcp.Description("v005-08: filesystem path to a bootgen boot-profile YAML. Carries the MCP server allowlist (mcp_servers).")),
		mcp.WithString("override", mcp.Description("v005-08: JSON object applied last over the resolved plan. Fields: system_prompt (string), env (KEY:VAL map).")),
		mcp.WithString("prompt_append", mcp.Description("Additional boot-prompt text appended after catalog/agent/override content. Use for narrow launch-time handoffs without replacing the base prompt.")),
		mcp.WithString("injection", mcp.Description("Caller-provided JSON config.LaunchInjection (native_files + boot_dir_overlay) supplied outside catalog YAML. Caller native files append after catalog native files; caller boot-dir overlay entries win on duplicate rel_path. SECURITY: persisted at rest in launch_plans — non-secret content only; route secrets through provider env passthrough/whitelist instead.")),
	), Writes(), a.handleSessionCreate)

	a.addTool(s, mcp.NewTool("mux_session_launch",
		mcp.WithDescription("Start a previously created session (transitions from created → running). Returns launch details including workspace path and log path."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID returned by mux_session_create")),
	), Writes(), a.handleSessionLaunch)

	a.addTool(s, mcp.NewTool("mux_session_stop",
		mcp.WithDescription("Send a stop signal to a running session."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
	), Destroys("terminates the running process; a stopped session cannot be relaunched, only resumed into a new one"), a.handleSessionStop)

	a.addTool(s, mcp.NewTool("mux_session_wait",
		mcp.WithDescription("Block until the session exits and return its exit code. Use after mux_session_stop or for short-lived sessions."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
	), Reads("GET /sessions/{id}/wait blocks on a state change it does not cause"), a.handleSessionWait)

	a.addTool(s, mcp.NewTool("mux_session_send_input",
		mcp.WithDescription("Send raw text input to a running session's stdin (PTY). Use to interact with a CLI agent session."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
		mcp.WithString("input", mcp.Required(), mcp.Description("Text to send to the session (a newline is NOT appended automatically)")),
	), Writes(), a.handleSessionSendInput)

	a.addTool(s, mcp.NewTool("mux_session_send_turn",
		mcp.WithDescription("Send a user turn to a running session with lifecycle-aware framing. Streaming-stdio sessions (Claude mode-5) receive an NDJSON user-message envelope; jsonrpc-stdio sessions (Codex app-server) get initialize+thread/start lazily followed by turn/start; PTY and unknown modes fall back to raw stdin. Prefer this over mux_session_send_input for long-lived agent turns — it removes per-call framing burden."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
		mcp.WithString("text", mcp.Required(), mcp.Description("User-facing message body. Framing is applied per the session's caps.")),
	), Writes(), a.handleSessionSendTurn)

	a.addTool(s, mcp.NewTool("mux_session_resize",
		mcp.WithDescription("Resize the PTY terminal for a running session."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
		mcp.WithNumber("rows", mcp.Required(), mcp.Description("Terminal rows (must be > 0)")),
		mcp.WithNumber("cols", mcp.Required(), mcp.Description("Terminal columns (must be > 0)")),
	), Writes(), a.handleSessionResize)

	a.addTool(s, mcp.NewTool("mux_session_health",
		mcp.WithDescription("Get the live runtime health snapshot for a running session. Returns provider identity, capability flags, and fine-grained live state (idle/processing/stopped). Returns not_found if the session does not exist, conflict if the session is not currently running."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID")),
	), Reads("svc.RuntimeHealth: live snapshot, no state change"), a.handleSessionHealth)
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

func (a *Adapter) handleSessionCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	launchID := str(req, "launch_id")
	if launchID == "" {
		return toolError("invalid_request", "launch_id required"), nil
	}
	bootPrompt := str(req, "boot_prompt")
	agentFile := str(req, "agent_file")
	agentInline := str(req, "agent_inline")
	bootProfile := str(req, "boot_profile")
	override := str(req, "override")
	promptAppend := str(req, "prompt_append")
	injection := str(req, "injection")

	if a.client != nil {
		// Daemon-routed path (production "mux mcp"): the daemon owns session
		// state, so creation must originate there. Otherwise the in-process
		// app would race against the daemon's session store.
		creq := api.LaunchRequest{
			Launch:          launchID,
			BootPrompt:      bootPrompt,
			AgentFile:       agentFile,
			AgentInline:     agentInline,
			BootProfileFile: bootProfile,
			Override:        override,
			PromptAppend:    promptAppend,
			Injection:       injection,
		}
		var res api.LaunchResponse
		var err error
		if agentFile != "" || agentInline != "" || bootProfile != "" || override != "" || injection != "" || promptAppend != "" {
			res, err = a.client.CreateSessionWithInput(ctx, creq)
		} else if bootPrompt != "" {
			res, err = a.client.CreateSessionWithBootPrompt(ctx, launchID, bootPrompt)
		} else {
			res, err = a.client.CreateSession(ctx, launchID)
		}
		if err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return toolError("internal_error", err.Error()), nil
		}
		return toolJSON(map[string]any{
			"ok":               true,
			"session_id":       res.ID,
			"workspace":        res.Workspace,
			"log":              res.Log,
			"provider_id":      res.ProviderID,
			"provider_kind":    res.ProviderKind,
			"logical_agent_id": res.LogicalAgentID,
		}), nil
	}

	// In-process path (tests, dev with no daemon).
	var res *app.Launched
	var err error
	if agentFile != "" || agentInline != "" || bootProfile != "" || override != "" || injection != "" || promptAppend != "" {
		res, err = a.svc.CreateSessionWithInput(app.CreateSessionInput{
			LaunchID:           launchID,
			BootPromptOverride: bootPrompt,
			AgentFile:          agentFile,
			AgentInline:        agentInline,
			BootProfileFile:    bootProfile,
			Override:           override,
			BootPromptAppend:   promptAppend,
			Injection:          injection,
		})
	} else if bootPrompt != "" {
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

func (a *Adapter) handleSessionLaunch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	if a.client != nil {
		res, err := a.client.LaunchSession(ctx, id)
		if err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return classifyClientErr(err, id), nil
		}
		return toolJSON(map[string]any{
			"ok":               true,
			"session_id":       res.ID,
			"workspace":        res.Workspace,
			"log":              res.Log,
			"provider_id":      res.ProviderID,
			"provider_kind":    res.ProviderKind,
			"logical_agent_id": res.LogicalAgentID,
		}), nil
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

func (a *Adapter) handleSessionStop(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	if a.client != nil {
		if err := a.client.StopSession(ctx, id); err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return classifyClientErr(err, id), nil
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id}), nil
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
	if a.client != nil {
		code, err := a.client.WaitSession(ctx, id)
		if err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return classifyClientErr(err, id), nil
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "exit_code": code}), nil
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

func (a *Adapter) handleSessionSendInput(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	if a.client != nil {
		if err := a.client.SendInput(ctx, id, []byte(input)); err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return classifyClientErr(err, id), nil
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(input)}), nil
	}
	if err := a.svc.SendInput(id, []byte(input)); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not found: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(input)}), nil
}

func (a *Adapter) handleSessionSendTurn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	text := str(req, "text")
	if text == "" {
		return toolError("invalid_request", "text required"), nil
	}
	if a.client != nil {
		if err := a.client.SendTurn(ctx, id, text); err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return classifyClientErr(err, id), nil
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(text)}), nil
	}
	if err := a.svc.SendTurn(ctx, id, text); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not found: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(text)}), nil
}

func (a *Adapter) handleSessionResize(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	if a.client != nil {
		if err := a.client.ResizeSession(ctx, id, rows, cols); err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return classifyClientErr(err, id), nil
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "rows": rows, "cols": cols}), nil
	}
	if err := a.svc.ResizeSession(id, rows, cols); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not running: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "rows": rows, "cols": cols}), nil
}

func (a *Adapter) handleSessionHealth(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := str(req, "session_id")
	if id == "" {
		return toolError("invalid_request", "session_id required"), nil
	}
	// Verify the session exists in the store.
	if _, err := a.svc.GetSession(id); err != nil {
		if isNotFound(err) {
			return toolError("not_found", "session not found: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	// Fetch live runtime health from the manager.
	result, ok := a.svc.RuntimeHealth(id)
	if !ok {
		return toolError("conflict", "session is not currently running; no live health available"), nil
	}
	caps := result.Caps
	data := map[string]any{
		"ok":            true,
		"session_id":    id,
		"alive":         result.Health.Alive,
		"live_state":    result.Health.State.String(),
		"turn_id":       result.Health.TurnID,
		"provider_id":   result.ProviderID,
		"provider_kind": result.ProviderKind,
		"caps": map[string]any{
			"pty":                 caps.PTY,
			"streaming_stdio":     caps.StreamingStdio,
			"jsonrpc_stdio":       caps.JsonRpcStdio, //nolint:staticcheck // mirrors lib's Caps.JsonRpcStdio field name
			"resize":              caps.Resize,
			"provider_session_id": caps.ProviderSessionID,
			"checkpoint_resume":   caps.CheckpointResume,
			"binary_required":     caps.BinaryRequired,
		},
	}
	if result.Health.PID != 0 {
		data["pid"] = result.Health.PID
	}
	return toolJSON(data), nil
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
		errors.Is(err, agentsessions.ErrSessionNotRunning) ||
		errors.Is(err, messaging.ErrNotFound)
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
