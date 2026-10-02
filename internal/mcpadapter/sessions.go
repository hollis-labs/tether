package mcpadapter

import (
	"context"
	"errors"
	"strings"

	"github.com/hollis-labs/agentkit/agentsessions"
	gomcp "github.com/hollis-labs/go-mcp/server"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-runner/runner"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func (a *Adapter) registerSessionTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_list",
		Description: "List agent sessions. Optionally filter by state (created, running, stopped, failed) and paginate with cursor and limit.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("state", "Filter by session state: created, running, stopped, failed", false),
			gomcp.StringProp("cursor", "RFC3339 pagination cursor — returns sessions older than this timestamp", false),
			gomcp.NumberProp("limit", "Max results (default 50, max 200)", false),
		),
		Handler: a.handleSessionList,
	}, Reads("GET /sessions; svc.ListSessions + AttachedClients"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_get",
		Description: "Get a single agent session by ID.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID", true),
		),
		Handler: a.handleSessionGet,
	}, Reads("GET /sessions/{id}; svc.GetSession + AttachedClients"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_create",
		Description: "Create a session from a launch profile (state=created, not yet running). Follow with tether_session_launch to start it. Supports v005-08 Agent Ops Tier-2 caller-provided payloads (agent_file / agent_inline / boot_profile / override / prompt_append) — when any are set, they merge over the catalog-resolved agent + boot profile.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("launch_id", "Launch profile ID from the catalog (see tether_catalog_list_launches)", true),
			gomcp.StringProp("boot_prompt", "Optional boot prompt override; replaces catalog static boot fragments verbatim", false),
			gomcp.StringProp("agent_file", "v005-08: filesystem path to an agent YAML matching config.Agent shape. Field-merged over the catalog agent.", false),
			gomcp.StringProp("agent_inline", "v005-08: JSON-encoded agent definition (same shape as config.Agent). Highest precedence in agent resolve order.", false),
			gomcp.StringProp("boot_profile", "v005-08: filesystem path to a bootgen boot-profile YAML. Carries the MCP server allowlist (mcp_servers).", false),
			gomcp.StringProp("override", "v005-08: JSON object applied last over the resolved plan. Fields: system_prompt (string), env (KEY:VAL map).", false),
			gomcp.StringProp("prompt_append", "Additional boot-prompt text appended after catalog/agent/override content. Use for narrow launch-time handoffs without replacing the base prompt.", false),
			gomcp.StringProp("idempotency_key", "Optional. Makes the create idempotent: a retry with the same key and the same request returns the session the first one created (replayed=true); the same key with a different request fails with idempotency_conflict. Keys are one global, unauthenticated space; prefix them (e.g. \"myapp/<run>/<step>\").", false),
			gomcp.StringProp("injection", "Caller-provided JSON config.LaunchInjection (native_files + boot_dir_overlay) supplied outside catalog YAML. Caller native files append after catalog native files; caller boot-dir overlay entries win on duplicate rel_path. SECURITY: persisted at rest in launch_plans — non-secret content only; route secrets through provider env passthrough/whitelist instead.", false),
		),
		Handler: a.handleSessionCreate,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_launch",
		Description: "Start a previously created session (transitions from created → running). Returns launch details including workspace path and log path.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID returned by tether_session_create", true),
		),
		Handler: a.handleSessionLaunch,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_stop",
		Description: "Send a stop signal to a running session.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID", true),
		),
		Handler: a.handleSessionStop,
	}, Destroys("terminates the running process; a stopped session cannot be relaunched, only resumed into a new one"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_wait",
		Description: "Block until the session exits and return its exit code. Use after tether_session_stop or for short-lived sessions.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID", true),
		),
		Handler: a.handleSessionWait,
	}, Reads("GET /sessions/{id}/wait blocks on a state change it does not cause"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_send_input",
		Description: "Send raw text input to a running session's stdin (PTY). Use to interact with a CLI agent session.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID", true),
			gomcp.StringProp("input", "Text to send to the session (a newline is NOT appended automatically)", true),
		),
		Handler: a.handleSessionSendInput,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_send_turn",
		Description: "Send a user turn to a running session with lifecycle-aware framing. Streaming-stdio sessions (Claude mode-5) receive an NDJSON user-message envelope; jsonrpc-stdio sessions (Codex app-server) get initialize+thread/start lazily followed by turn/start; PTY and unknown modes fall back to raw stdin. Prefer this over tether_session_send_input for long-lived agent turns — it removes per-call framing burden. A provider_session_lost error means the provider no longer has the session's resume id: the turn was not delivered, and resending starts a fresh provider session without the old history.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID", true),
			gomcp.StringProp("text", "User-facing message body. Framing is applied per the session's caps.", true),
		),
		Handler: a.handleSessionSendTurn,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_resize",
		Description: "Resize the PTY terminal for a running session.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID", true),
			gomcp.NumberProp("rows", "Terminal rows (must be > 0)", true),
			gomcp.NumberProp("cols", "Terminal columns (must be > 0)", true),
		),
		Handler: a.handleSessionResize,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_session_health",
		Description: "Get the live runtime health snapshot for a running session. Returns provider identity, capability flags, and fine-grained live state (idle/processing/stopped). Returns not_found if the session does not exist, conflict if the session is not currently running.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("session_id", "Session UUID", true),
		),
		Handler: a.handleSessionHealth,
	}, Reads("svc.RuntimeHealth: live snapshot, no state change"))
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleSessionList(ctx context.Context, args map[string]any) (any, error) {
	limit := intArg(args, "limit", 50)
	if limit > 200 {
		limit = 200
	}
	if a.readsViaDaemon() {
		res, err := a.client.ListSessions(ctx, client.ListOptions{State: str(args, "state"), Cursor: str(args, "cursor"), Limit: limit})
		if err != nil {
			return nil, daemonReadError(err, "")
		}
		out := map[string]any{
			"ok":       true,
			"sessions": res.Sessions,
			"count":    len(res.Sessions),
		}
		if res.NextCursor != "" {
			out["next_cursor"] = res.NextCursor
		}
		return toolJSON(out), nil
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

func (a *Adapter) handleSessionGet(ctx context.Context, args map[string]any) (any, error) {
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	if a.readsViaDaemon() {
		dto, err := a.client.GetSession(ctx, id)
		if err != nil {
			return nil, daemonReadError(err, id)
		}
		return toolJSON(map[string]any{"ok": true, "session": dto}), nil
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
	idempotencyKey := str(args, "idempotency_key")
	// A keyed request always takes the input path, where the key is recorded.
	withInput := idempotencyKey != "" || agentFile != "" || agentInline != "" || bootProfile != "" || override != "" || injection != "" || promptAppend != ""

	if a.client != nil {
		// Daemon-routed path (production "tether mcp"): the daemon owns session
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
			IdempotencyKey:  idempotencyKey,
		}
		var res api.LaunchResponse
		var err error
		if withInput {
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
			if strings.Contains(err.Error(), "(idempotency_conflict)") {
				return nil, toolError("idempotency_conflict", err.Error())
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
			"replayed":         res.Replayed,
		}), nil
	}

	// In-process path (tests, dev with no daemon).
	var res *app.Launched
	var err error
	if withInput {
		res, err = a.svc.CreateSessionWithInput(app.CreateSessionInput{
			LaunchID:           launchID,
			BootPromptOverride: bootPrompt,
			AgentFile:          agentFile,
			AgentInline:        agentInline,
			BootProfileFile:    bootProfile,
			Override:           override,
			BootPromptAppend:   promptAppend,
			Injection:          injection,
			IdempotencyKey:     idempotencyKey,
		})
	} else if bootPrompt != "" {
		res, err = a.svc.CreateSessionWithBootPrompt(launchID, bootPrompt)
	} else {
		res, err = a.svc.CreateSession(launchID)
	}
	if err != nil {
		if errors.Is(err, store.ErrIdempotencyConflict) {
			return nil, toolError("idempotency_conflict", err.Error())
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
		"replayed":         res.Replayed,
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
	res, err := a.svc.LaunchSessionWithContext(ctx, id)
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
		if errors.Is(err, gop.ErrProviderSessionLost) {
			return nil, toolError("provider_session_lost", err.Error())
		}
		var exit *runner.ExitError
		if errors.As(err, &exit) {
			return nil, toolError("turn_failed", "turn failed: "+err.Error())
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
		if errors.Is(err, gop.ErrProviderSessionLost) {
			return nil, toolError("provider_session_lost", err.Error())
		}
		var exit *runner.ExitError
		if errors.As(err, &exit) {
			return nil, toolError("turn_failed", "turn failed: "+err.Error())
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

func (a *Adapter) handleSessionHealth(ctx context.Context, args map[string]any) (any, error) {
	id := str(args, "session_id")
	if id == "" {
		return nil, toolError("invalid_request", "session_id required")
	}
	if a.readsViaDaemon() {
		h, err := a.client.SessionHealth(ctx, id)
		if err != nil {
			return nil, daemonReadError(err, id)
		}
		return toolJSON(sessionHealthData(id, h.Alive, h.PID, h.LiveState, h.TurnID, h.ProviderID, h.ProviderKind, h.Caps)), nil
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
	return toolJSON(sessionHealthData(id, result.Health.Alive, result.Health.PID, result.Health.State.String(), result.Health.TurnID, result.ProviderID, result.ProviderKind, api.CapabilitiesDTO{
		PTY:               caps.PTY,
		StreamingStdio:    caps.StreamingStdio,
		JSONRPCStdio:      caps.JsonRpcStdio, //nolint:staticcheck // mirrors lib's Caps.JsonRpcStdio field name
		Resize:            caps.Resize,
		ProviderSessionID: caps.ProviderSessionID,
		CheckpointResume:  caps.CheckpointResume,
		BinaryRequired:    caps.BinaryRequired,
	})), nil
}

// sessionHealthData is tether_session_health's result, from the daemon's
// health response or the in-process runtime alike.
func sessionHealthData(id string, alive bool, pid int, liveState, turnID, providerID, providerKind string, caps api.CapabilitiesDTO) map[string]any {
	data := map[string]any{
		"ok":            true,
		"session_id":    id,
		"alive":         alive,
		"live_state":    liveState,
		"turn_id":       turnID,
		"provider_id":   providerID,
		"provider_kind": providerKind,
		"caps": map[string]any{
			"pty":                 caps.PTY,
			"streaming_stdio":     caps.StreamingStdio,
			"jsonrpc_stdio":       caps.JSONRPCStdio,
			"resize":              caps.Resize,
			"provider_session_id": caps.ProviderSessionID,
			"checkpoint_resume":   caps.CheckpointResume,
			"binary_required":     caps.BinaryRequired,
		},
	}
	if pid != 0 {
		data["pid"] = pid
	}
	return data
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
