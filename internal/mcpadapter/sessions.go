package mcpadapter

import (
	"context"
	"errors"

	"github.com/hollis-labs/agentkit/agentsessions"
	gomcp "github.com/hollis-labs/go-mcp/server"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func (a *Adapter) registerSessionTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_list",
		Description: "List agent sessions. Optionally filter by state (created, running, stopped, failed) and paginate with cursor and limit.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"state":  strProp("Filter by session state: created, running, stopped, failed"),
			"cursor": strProp("RFC3339 pagination cursor — returns sessions older than this timestamp"),
			"limit":  numProp("Max results (default 50, max 200)"),
		}),
		Handler: a.handleSessionList,
	}, Reads("GET /sessions; svc.ListSessions + AttachedClients"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_get",
		Description: "Get a single agent session by ID.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
		}, "session_id"),
		Handler: a.handleSessionGet,
	}, Reads("GET /sessions/{id}; svc.GetSession + AttachedClients"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_create",
		Description: "Create a session from a launch profile (state=created, not yet running). Follow with mux_session_launch to start it. Supports v005-08 Agent Ops Tier-2 caller-provided payloads (agent_file / agent_inline / boot_profile / override / prompt_append) — when any are set, they merge over the catalog-resolved agent + boot profile.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"launch_id":     strProp("Launch profile ID from the catalog (see mux_catalog_list_launches)"),
			"boot_prompt":   strProp("Optional boot prompt override; replaces catalog static boot fragments verbatim"),
			"agent_file":    strProp("v005-08: filesystem path to an agent YAML matching config.Agent shape. Field-merged over the catalog agent."),
			"agent_inline":  strProp("v005-08: JSON-encoded agent definition (same shape as config.Agent). Highest precedence in agent resolve order."),
			"boot_profile":  strProp("v005-08: filesystem path to a bootgen boot-profile YAML. Carries the MCP server allowlist (mcp_servers)."),
			"override":      strProp("v005-08: JSON object applied last over the resolved plan. Fields: system_prompt (string), env (KEY:VAL map)."),
			"prompt_append": strProp("Additional boot-prompt text appended after catalog/agent/override content. Use for narrow launch-time handoffs without replacing the base prompt."),
			"injection":     strProp("Caller-provided JSON config.LaunchInjection (native_files + boot_dir_overlay) supplied outside catalog YAML. Caller native files append after catalog native files; caller boot-dir overlay entries win on duplicate rel_path. SECURITY: persisted at rest in launch_plans — non-secret content only; route secrets through provider env passthrough/whitelist instead."),
		}, "launch_id"),
		Handler: a.handleSessionCreate,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_launch",
		Description: "Start a previously created session (transitions from created → running). Returns launch details including workspace path and log path.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID returned by mux_session_create"),
		}, "session_id"),
		Handler: a.handleSessionLaunch,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_stop",
		Description: "Send a stop signal to a running session.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
		}, "session_id"),
		Handler: a.handleSessionStop,
	}, Destroys("terminates the running process; a stopped session cannot be relaunched, only resumed into a new one"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_wait",
		Description: "Block until the session exits and return its exit code. Use after mux_session_stop or for short-lived sessions.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
		}, "session_id"),
		Handler: a.handleSessionWait,
	}, Reads("GET /sessions/{id}/wait blocks on a state change it does not cause"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_send_input",
		Description: "Send raw text input to a running session's stdin (PTY). Use to interact with a CLI agent session.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
			"input":      strProp("Text to send to the session (a newline is NOT appended automatically)"),
		}, "session_id", "input"),
		Handler: a.handleSessionSendInput,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_send_turn",
		Description: "Send a user turn to a running session with lifecycle-aware framing. Streaming-stdio sessions (Claude mode-5) receive an NDJSON user-message envelope; jsonrpc-stdio sessions (Codex app-server) get initialize+thread/start lazily followed by turn/start; PTY and unknown modes fall back to raw stdin. Prefer this over mux_session_send_input for long-lived agent turns — it removes per-call framing burden.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
			"text":       strProp("User-facing message body. Framing is applied per the session's caps."),
		}, "session_id", "text"),
		Handler: a.handleSessionSendTurn,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_resize",
		Description: "Resize the PTY terminal for a running session.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
			"rows":       numProp("Terminal rows (must be > 0)"),
			"cols":       numProp("Terminal columns (must be > 0)"),
		}, "session_id", "rows", "cols"),
		Handler: a.handleSessionResize,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_health",
		Description: "Get the live runtime health snapshot for a running session. Returns provider identity, capability flags, and fine-grained live state (idle/processing/stopped). Returns not_found if the session does not exist, conflict if the session is not currently running.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
		}, "session_id"),
		Handler: a.handleSessionHealth,
	}, Reads("svc.RuntimeHealth: live snapshot, no state change"))
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleSessionList(_ context.Context, args map[string]any) (any, error) {
	limit := intArg(args, "limit", 50)
	if limit > 200 {
		limit = 200
	}
	opts := store.ListSessionsOptions{
		State:  str(args, "state"),
		Cursor: str(args, "cursor"),
		Limit:  limit,
	}
	rows, err := a.svc.ListSessions(opts)
	if err != nil {
		return nil, toolError("internal_error", err.Error())
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

func (a *Adapter) handleSessionGet(_ context.Context, args map[string]any) (any, error) {
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	row, err := a.svc.GetSession(id)
	if err != nil {
		if isNotFound(err) {
			return nil, toolError("not_found", "session not found: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	dto := api.SessionRowToDTO(*row)
	dto.AttachedClients = a.svc.AttachedClients(id)
	return toolJSON(map[string]any{"ok": true, "session": dto}), nil
}

func (a *Adapter) handleSessionCreate(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeSessionWrite); err != nil {
		return nil, err
	}
	launchID := str(args, "launch_id")
	if launchID == "" {
		return nil, toolError("invalid_request", "launch_id required")
	}
	bootPrompt := str(args, "boot_prompt")
	agentFile := str(args, "agent_file")
	agentInline := str(args, "agent_inline")
	bootProfile := str(args, "boot_profile")
	override := str(args, "override")
	promptAppend := str(args, "prompt_append")
	injection := str(args, "injection")

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
				return nil, daemonUnreachableError(err)
			}
			return nil, toolError("internal_error", err.Error())
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
		return nil, toolError("internal_error", err.Error())
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

func (a *Adapter) handleSessionLaunch(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeSessionWrite); err != nil {
		return nil, err
	}
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	if a.client != nil {
		res, err := a.client.LaunchSession(ctx, id)
		if err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, classifyClientErr(err, id)
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
			return nil, toolError("not_found", "session not found: "+id)
		}
		if isConflict(err) {
			return nil, toolError("conflict", err.Error())
		}
		return nil, toolError("internal_error", err.Error())
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

func (a *Adapter) handleSessionStop(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeSessionWrite); err != nil {
		return nil, err
	}
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	if a.client != nil {
		if err := a.client.StopSession(ctx, id); err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, classifyClientErr(err, id)
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id}), nil
	}
	if err := a.svc.StopSession(id); err != nil {
		if isNotFound(err) {
			return nil, toolError("not_found", "session not running: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id}), nil
}

func (a *Adapter) handleSessionWait(ctx context.Context, args map[string]any) (any, error) {
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	if a.client != nil {
		code, err := a.client.WaitSession(ctx, id)
		if err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, classifyClientErr(err, id)
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "exit_code": code}), nil
	}
	code, err := a.svc.WaitSession(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, toolError("not_found", "session not running: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "exit_code": code}), nil
}

func (a *Adapter) handleSessionSendInput(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeSessionWrite); err != nil {
		return nil, err
	}
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	input := str(args, "input")
	if input == "" {
		return nil, toolError("invalid_request", "input required")
	}
	if a.client != nil {
		if err := a.client.SendInput(ctx, id, []byte(input)); err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, classifyClientErr(err, id)
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(input)}), nil
	}
	if err := a.svc.SendInput(id, []byte(input)); err != nil {
		if isNotFound(err) {
			return nil, toolError("not_found", "session not found: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(input)}), nil
}

func (a *Adapter) handleSessionSendTurn(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeSessionWrite); err != nil {
		return nil, err
	}
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	text := str(args, "text")
	if text == "" {
		return nil, toolError("invalid_request", "text required")
	}
	if a.client != nil {
		if err := a.client.SendTurn(ctx, id, text); err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, classifyClientErr(err, id)
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(text)}), nil
	}
	if err := a.svc.SendTurn(ctx, id, text); err != nil {
		if isNotFound(err) {
			return nil, toolError("not_found", "session not found: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "bytes_sent": len(text)}), nil
}

func (a *Adapter) handleSessionResize(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeSessionWrite); err != nil {
		return nil, err
	}
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	rowsInt := intArg(args, "rows", 0)
	colsInt := intArg(args, "cols", 0)
	if rowsInt <= 0 || colsInt <= 0 || rowsInt > 65535 || colsInt > 65535 {
		return nil, toolError("invalid_request", "rows and cols must be between 1 and 65535")
	}
	rows := uint16(rowsInt) //nolint:gosec // range validated above
	cols := uint16(colsInt) //nolint:gosec // range validated above
	if a.client != nil {
		if err := a.client.ResizeSession(ctx, id, rows, cols); err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, classifyClientErr(err, id)
		}
		return toolJSON(map[string]any{"ok": true, "session_id": id, "rows": rows, "cols": cols}), nil
	}
	if err := a.svc.ResizeSession(id, rows, cols); err != nil {
		if isNotFound(err) {
			return nil, toolError("not_found", "session not running: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(map[string]any{"ok": true, "session_id": id, "rows": rows, "cols": cols}), nil
}

func (a *Adapter) handleSessionHealth(_ context.Context, args map[string]any) (any, error) {
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	// Verify the session exists in the store.
	if _, err := a.svc.GetSession(id); err != nil {
		if isNotFound(err) {
			return nil, toolError("not_found", "session not found: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	// Fetch live runtime health from the manager.
	result, ok := a.svc.RuntimeHealth(id)
	if !ok {
		return nil, toolError("conflict", "session is not currently running; no live health available")
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
