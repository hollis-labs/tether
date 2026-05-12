package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/chrispian/agent-mux/internal/agent"
	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/broker"
	"github.com/chrispian/agent-mux/internal/checkpoint"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/events"
	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/provider/api/stub"
	"github.com/chrispian/agent-mux/internal/provider/cli/claudestream"
	"github.com/chrispian/agent-mux/internal/provider/cli/opencode"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/store"
	"github.com/chrispian/agent-mux/internal/workspace"
)

// RuntimeFactory builds an agentsessions.Runtime for a single launch. The
// plan supplies binary path, args, and env policy; per-launch StartOptions
// supply workspace, log path, sandbox profile, and fanout (passed at
// Manager.Start time).
type RuntimeFactory func(plan *launch.Plan) (agentsessions.Runtime, error)

// Service is the composition root: it wires catalog, store, sinks, and
// the agentsessions.Manager. Lifecycle ownership of running sessions
// lives in agentsessions; Service only orchestrates launch preconditions
// (plan resolution, workspace creation, persistence of the initial row)
// and then hands the handle off to the manager.
type Service struct {
	CatalogRoot string
	Catalog     *config.Catalog
	Store       *store.Store
	Manager     *agentsessions.Manager
	Bus         events.Bus
	Broker      *broker.Service

	factories map[string]RuntimeFactory

	// codexThreads caches the JSON-RPC thread id per session id for
	// JsonRpcStdio runtimes. Populated lazily on the first SendTurn call
	// for a session (after initialize + thread/start succeed).
	codexThreads sync.Map // map[string]string
}

func New(catalogRoot string) (*Service, error) {
	cat, err := config.Load(catalogRoot)
	if err != nil {
		return nil, err
	}
	if err := cat.Validate(); err != nil {
		return nil, err
	}
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	if dbPath == "" {
		return nil, fmt.Errorf("global.defaults.state_db missing")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}

	factories := map[string]RuntimeFactory{
		"claude-stream":    claudestream.New,
		"claude-code":      newClaudeCodeRuntime,
		"codex-app-server": newCodexAppServerRuntime,
		"opencode":         opencode.New,
		"api-stub":         stub.New,
	}
	// Register cli-goprovider runtimes declared in the catalog. Each one
	// is the same pattern as claude-stream — a per-turn subprocess driven
	// by a go-providers CLIAdapter — so claudestream.NewWithAdapter is
	// the shared constructor. Catalog ids that match a built-in name
	// override the built-in (last write wins, matching the legacy
	// provider.Registry semantics).
	for _, p := range cat.Providers {
		if p.Type != "cli-goprovider" {
			continue
		}
		adapter := goproviderCLIAdapter(p.Adapter)
		if adapter == nil {
			continue
		}
		providerID := p.ID
		ad := adapter
		factories[providerID] = func(plan *launch.Plan) (agentsessions.Runtime, error) {
			return claudestream.NewWithAdapter(plan, ad, providerID, agentsessions.Capabilities{
				PTY:               false,
				Resize:            false,
				ProviderSessionID: true,
				CheckpointResume:  false,
				BinaryRequired:    true,
			})
		}
	}

	// Seed logical_agents from the catalog. Upsert — idempotent across
	// restarts; preserves created_at + any future operator-set fields.
	// See ADR 0003.
	if n, err := seedLogicalAgents(db, cat.Agents); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seed logical_agents: %w", err)
	} else if n > 0 {
		log.Printf("store: seeded %d logical_agent row(s) from catalog", n)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})

	evSink := &eventSinkAdapter{bus: bus}
	mgr := agentsessions.NewManager(stateSinkAdapter{db: db}).
		WithAttachmentSink(attachmentSinkAdapter{db: db}).
		WithEventSink(evSink)
	evSink.SetManager(mgr)

	brk := broker.NewService(db, bus)
	return &Service{
		CatalogRoot: catalogRoot,
		Catalog:     cat,
		Store:       db,
		Manager:     mgr,
		Bus:         bus,
		Broker:      brk,
		factories:   factories,
	}, nil
}

// ReconcileStaleState sweeps any sessions stuck in launching/running and
// any open client_attachments to terminal state. Intended for daemon
// startup only — `mux mcp` and other catalog-reading subcommands MUST
// NOT call this, because they may run concurrently with a live daemon
// (e.g. when a session spawns mux mcp as an MCP subprocess), and
// sweeping would clobber the daemon's actively-tracked sessions. See
// ADR 0030 §sweep-race for the original incident.
func (s *Service) ReconcileStaleState() {
	now := time.Now().UTC().Format(time.RFC3339)
	if swept, err := s.Store.SweepStaleSessions(now); err == nil && swept > 0 {
		log.Printf("store: swept %d stale session(s) to failed", swept)
	}
	if swept, err := s.Store.SweepStaleAttachments(now); err == nil && swept > 0 {
		log.Printf("store: swept %d stale client_attachments row(s)", swept)
	}
}

func (s *Service) Close() error {
	if err := s.Manager.Shutdown(context.Background()); err != nil {
		return err
	}
	return s.Store.Close()
}

func newClaudeCodeRuntime(plan *launch.Plan) (agentsessions.Runtime, error) {
	adapter := gop.NewClaudeAdapterStreamingStdio()
	adapter.ApiKeyHelperPath = resolveAPIKeyHelperPath()
	return claudestream.NewWithAdapter(plan, adapter, "claude-code", agentsessions.Capabilities{
		StreamingStdio:    true,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	})
}

func newCodexAppServerRuntime(plan *launch.Plan) (agentsessions.Runtime, error) {
	return claudestream.NewWithAdapter(plan, gop.NewCodexAdapterAppServer(), "codex-app-server", agentsessions.Capabilities{
		JsonRpcStdio:     true,
		CheckpointResume: false,
		BinaryRequired:   true,
	})
}

func (s *Service) ListProjects() []config.Project {
	out := make([]config.Project, 0, len(s.Catalog.Projects))
	for _, p := range s.Catalog.Projects {
		out = append(out, p)
	}
	return out
}

func (s *Service) ListAgents() []config.Agent {
	out := make([]config.Agent, 0, len(s.Catalog.Agents))
	for _, a := range s.Catalog.Agents {
		out = append(out, a)
	}
	return out
}

func (s *Service) ListProviders() []config.Provider {
	out := make([]config.Provider, 0, len(s.Catalog.Providers))
	for _, p := range s.Catalog.Providers {
		out = append(out, p)
	}
	return out
}

func (s *Service) Resolve(launchID string) (*launch.Plan, error) {
	return launch.Resolve(s.Catalog, launch.Input{LaunchID: launchID, CatalogRoot: s.CatalogRoot})
}

type Launched struct {
	SessionID string
	Workspace *workspace.Session
	Plan      *launch.Plan

	// Wait blocks until the session reaches a terminal state and returns
	// its exit code. Routes through agentsessions.Manager; safe to call
	// from any goroutine, safe to call after the session has already
	// exited. Nil on the result of CreateSession — launching first is
	// required before there's anything to wait on.
	Wait func(ctx context.Context) (int, error)

	// ProviderKind is the runtime family ("cli" | "api"), resolved from
	// the runtime factory at create/launch time. Populated as part of
	// ADR 0022 G2/G3 so consumers don't need a follow-up catalog lookup.
	ProviderKind string
}

// ErrSessionNotCreated is returned by LaunchSession when the target
// session is in any state other than "created". Once launched, a
// session cannot be launched again — clients create a new session for
// a retry.
//
// Deprecated: prefer session.ErrNotCreated; this alias is kept so
// existing callers that reference app.ErrSessionNotCreated do not break.
var ErrSessionNotCreated = session.ErrNotCreated

// CreateSessionWithBootPrompt creates a session like CreateSession but
// replaces the catalog's static boot prompt with the provided text.
// Used by `mux boot <profile_id>` to inject a dynamically generated
// prompt without modifying the catalog.
func (s *Service) CreateSessionWithBootPrompt(launchID, bootPrompt string) (*Launched, error) {
	plan, err := s.Resolve(launchID)
	if err != nil {
		return nil, err
	}
	if bootPrompt != "" {
		plan.BootPrompt = bootPrompt
	}
	return s.createSessionFromPlan(plan)
}

// CreateSession resolves the launch plan, materializes the workspace,
// and persists the session row in state=created along with the plan
// JSON. It does not start the runtime — call LaunchSession for that.
func (s *Service) CreateSession(launchID string) (*Launched, error) {
	plan, err := s.Resolve(launchID)
	if err != nil {
		return nil, err
	}
	return s.createSessionFromPlan(plan)
}

func (s *Service) createSessionFromPlan(plan *launch.Plan) (*Launched, error) {
	sessID := uuid.NewString()

	wsRoot := plan.WriteHome
	if wsRoot == "" {
		wsRoot = filepath.Join(config.Expand(s.Catalog.Global.Catalog.Defaults.WorkspaceRoot), plan.ProjectID)
	}
	ws, err := workspace.Create(wsRoot, sessID, plan)
	if err != nil {
		return nil, err
	}

	// Fail fast if the catalog references a provider the factory map
	// can't satisfy. Better to error here than at launch time when the
	// user thinks they have a created session.
	factory, ok := s.factories[plan.ProviderID]
	if !ok {
		return nil, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}
	probe, err := factory(plan)
	if err != nil {
		return nil, fmt.Errorf("build runtime for %q: %w", plan.ProviderID, err)
	}
	providerKind := probe.Kind()

	row := store.SessionRow{
		ID:             sessID,
		LaunchID:       plan.LaunchID,
		ProjectID:      plan.ProjectID,
		LogicalAgentID: plan.LogicalAgentID,
		ProviderID:     plan.ProviderID,
		ProviderKind:   providerKind,
		Workspace:      ws.Root,
		State:          string(session.StateCreated),
	}
	if err := s.Store.CreateSession(row, plan); err != nil {
		return nil, err
	}

	return &Launched{
		SessionID:    sessID,
		Workspace:    ws,
		Plan:         plan,
		ProviderKind: providerKind,
	}, nil
}

// LaunchSession starts a previously-created session. Rehydrates the
// plan from the store, opens the workspace non-destructively, runs
// Runtime.Prepare and hands off to agentsessions.Manager.Start.
// Returns ErrSessionNotCreated when the target is in any state other
// than "created" — relaunch of terminated sessions is not supported.
func (s *Service) LaunchSession(sessionID string) (*Launched, error) {
	row, err := s.Store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if row.State != string(session.StateCreated) {
		return nil, fmt.Errorf("%w (state=%q)", ErrSessionNotCreated, row.State)
	}

	plan, err := s.Store.GetLaunchPlan(sessionID)
	if err != nil {
		return nil, fmt.Errorf("load launch plan: %w", err)
	}

	factory, ok := s.factories[plan.ProviderID]
	if !ok {
		return nil, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}
	rt, err := factory(plan)
	if err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessionID, string(session.StateFailed), 0, &exit)
		return nil, fmt.Errorf("build runtime: %w", err)
	}

	ws := workspace.Open(row.Workspace, sessionID)

	if err := rt.Prepare(context.Background()); err != nil {
		exit := 1
		_ = s.Store.UpdateSessionState(sessionID, string(session.StateFailed), 0, &exit)
		return nil, err
	}

	// Resolve sandbox profile if the agent specifies one.
	var profile sandbox.Profile
	if a, ok := s.Catalog.Agents[plan.LogicalAgentID]; ok {
		if name := a.Permissions.DefaultSandbox; name != "" {
			if sp, ok := s.Catalog.SandboxProfiles[name]; ok {
				profile = sp
				if profile.ID == "workspace-plus-net" {
					profile.AllowLoopback = true
				}
			}
		}
	}

	// Resume continuity for turn-based providers that use a CLI session ID
	// (--resume / --session). Adapters that don't use this are inert.
	var sessionIDPreset string
	var onSessionID func(string)
	if providerHasSessionIDContinuity(plan.ProviderID) {
		if preset, err := s.Store.GetClaudeSessionID(plan.LogicalAgentID); err == nil {
			sessionIDPreset = preset
		}
		logicalAgentID := plan.LogicalAgentID
		storeRef := s.Store
		providerID := plan.ProviderID
		onSessionID = func(id string) {
			if err := storeRef.SetClaudeSessionID(logicalAgentID, id); err != nil {
				log.Printf("%s: persist session_id for %q failed: %v", providerID, logicalAgentID, err)
			}
		}
	}

	req := agentsessions.StartRequest{
		ID:      sessionID,
		Runtime: rt,
		Options: agentsessions.StartOptions{
			Workdir:         plan.RepoRoot,
			WorkspaceDir:    ws.Root,
			LogPath:         ws.LogPath,
			BootPrompt:      plan.BootPrompt,
			BootMode:        plan.BootMode,
			Env:             provider.BuildEnv(plan.EnvMode, plan.EnvPassthrough, plan.EnvRedact, plan.Env, os.Environ()),
			Profile:         profile,
			SessionIDPreset: sessionIDPreset,
			OnSessionID:     onSessionID,
			AttachEnabled:   true,
		},
		SessionMeta: map[string]string{
			"logical_agent_id": plan.LogicalAgentID,
			"project_id":       plan.ProjectID,
			"launch_id":        plan.LaunchID,
			"provider_id":      plan.ProviderID,
		},
	}
	if err := s.Manager.Start(context.Background(), req); err != nil {
		return nil, err
	}

	// Record this launch profile on the logical agent so the resume
	// endpoint knows which catalog config to use next time.
	if err := s.Store.SetLogicalAgentLaunchID(plan.LogicalAgentID, plan.LaunchID); err != nil {
		log.Printf("app: set launch_id on logical_agent %q: %v (non-fatal)", plan.LogicalAgentID, err)
	}

	return &Launched{
		SessionID:    sessionID,
		Workspace:    ws,
		Plan:         plan,
		ProviderKind: rt.Kind(),
		Wait: func(ctx context.Context) (int, error) {
			return s.Manager.WaitSession(ctx, sessionID)
		},
	}, nil
}

func (s *Service) ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error) {
	return s.Store.ListSessions(opts)
}

func (s *Service) GetSession(id string) (*store.SessionRow, error) {
	return s.Store.GetSession(id)
}

func (s *Service) StopSession(id string) error {
	return s.Manager.Stop(context.Background(), id)
}

// WaitSession blocks until the named session reaches a terminal state and
// returns its exit code. Thin wrapper over agentsessions.Manager.WaitSession
// so callers (including the daemon's HTTP handlers) don't reach past Service
// into the lib.
func (s *Service) WaitSession(ctx context.Context, id string) (int, error) {
	return s.Manager.WaitSession(ctx, id)
}

// SendInput writes data to the named session's input channel. Thin wrapper
// over agentsessions.Manager.SendInput.
func (s *Service) SendInput(id string, data []byte) error {
	return s.Manager.SendInput(id, data)
}

// SendTurn delivers a user message to the named session, applying the
// per-runtime framing required by the session's lifecycle mode:
//
//   - StreamingStdio (Claude): wraps text as
//     {"type":"user","message":{"role":"user","content":"<text>"}}\n
//     and writes to stdin.
//   - JsonRpcStdio (Codex app-server): performs lazy initialize +
//     thread/start on first call (caching the thread id per session id),
//     then issues turn/start with the cached thread id and the text input.
//   - PTY or unknown: falls back to raw SendInput([]byte(text)) so PTY
//     consumers still work without per-call framing.
//
// Existing SendInput callers are unaffected — SendTurn is additive and
// intended for callers that want lifecycle-aware framing without
// hand-rolling the per-mode envelope.
func (s *Service) SendTurn(ctx context.Context, id, text string) error {
	info, ok := s.Manager.Get(id)
	if !ok {
		return agentsessions.ErrSessionNotRunning
	}
	switch {
	case info.Caps.StreamingStdio:
		payload, err := frameUserMessage(text)
		if err != nil {
			return err
		}
		return s.Manager.SendInput(id, payload)
	case info.Caps.JsonRpcStdio:
		return s.sendTurnJSONRPC(ctx, id, text)
	default:
		return s.Manager.SendInput(id, []byte(text))
	}
}

// frameUserMessage encodes the NDJSON user-message envelope Claude's
// streaming-input mode (mode-5) expects on stdin. The envelope shape is
// {"type":"user","message":{"role":"user","content":"<text>"}} with a
// trailing newline. Exported for unit testing; SendTurn is the public
// caller.
func frameUserMessage(text string) ([]byte, error) {
	payload, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode user message: %w", err)
	}
	return append(payload, '\n'), nil
}

// sendTurnJSONRPC implements the JSON-RPC turn delivery for codex
// app-server style runtimes. Routes through Manager.JsonRpcCall (added
// in go-agent-sessions v0.9.0) so the raw Session reference stays
// hidden behind the Manager surface. Lazily runs initialize +
// thread/start on the first call for a session, caches the thread id,
// and issues turn/start with the cached id + user input.
func (s *Service) sendTurnJSONRPC(ctx context.Context, id, text string) error {
	threadID, cached := s.codexThreads.Load(id)
	if !cached {
		if _, err := s.Manager.JsonRpcCall(ctx, id, "initialize", map[string]any{}); err != nil {
			return fmt.Errorf("jsonrpc initialize: %w", err)
		}
		startRes, err := s.Manager.JsonRpcCall(ctx, id, "thread/start", map[string]any{})
		if err != nil {
			return fmt.Errorf("jsonrpc thread/start: %w", err)
		}
		var parsed struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(startRes, &parsed); err != nil {
			return fmt.Errorf("decode thread/start response: %w", err)
		}
		if parsed.ThreadID == "" {
			return fmt.Errorf("thread/start returned empty threadId")
		}
		threadID = parsed.ThreadID
		s.codexThreads.Store(id, threadID)
	}
	if _, err := s.Manager.JsonRpcCall(ctx, id, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    text,
	}); err != nil {
		return fmt.Errorf("jsonrpc turn/start: %w", err)
	}
	return nil
}

// ResizeSession forwards a (rows, cols) winsize update. Thin wrapper over
// agentsessions.Manager.Resize.
func (s *Service) ResizeSession(id string, rows, cols uint16) error {
	return s.Manager.Resize(id, rows, cols)
}

// AttachSession streams the named session's live output to w until ctx is
// canceled or the session exits. sinceSeq is a byte-offset hint for resume;
// 0 means "replay full ring then go live". Thin wrapper over
// agentsessions.Manager.AttachWith.
func (s *Service) AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error {
	return s.Manager.AttachWith(ctx, id, w, agentsessions.AttachOptions{SinceSeq: sinceSeq})
}

// AttachedClients reports the in-memory count of live attach subscribers
// for id, or 0 if the session is not currently registered.
func (s *Service) AttachedClients(id string) int {
	info, ok := s.Manager.Get(id)
	if !ok {
		return 0
	}
	return info.AttachedClients
}

// RuntimeHealth returns the live health snapshot for a running session by
// delegating to agentsessions.Manager.Health. Returns (zero, false) when
// the session is not currently registered.
func (s *Service) RuntimeHealth(id string) (api.RuntimeHealthResult, bool) {
	snap, ok := s.Manager.Health(id)
	if !ok {
		return api.RuntimeHealthResult{}, false
	}
	return api.RuntimeHealthResult{
		SessionID:    snap.SessionID,
		ProviderID:   snap.RuntimeID,
		ProviderKind: snap.RuntimeKind,
		Caps:         snap.Caps,
		Health:       snap.Health,
	}, true
}

// ResumeLogicalAgent starts a new session for the given logical agent
// using its most recent checkpoint as boot context. The launch profile
// from the agent's most recent previous session (logical_agents.launch_id)
// is reused.
//
// Returns a conflict error if the agent has never launched (no launch_id).
// Returns a not-found-shaped error if no checkpoint exists.
func (s *Service) ResumeLogicalAgent(logicalAgentID string) (api.LaunchResult, error) {
	la, err := s.Store.GetLogicalAgent(logicalAgentID)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("get logical agent: %w", err)
	}
	if la.LaunchID == "" {
		return api.LaunchResult{}, fmt.Errorf("agent %q has never launched a session; cannot resume", logicalAgentID)
	}

	ck, err := s.Store.GetLatestCheckpointForAgent(logicalAgentID)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("get latest checkpoint: %w", err)
	}

	plan, err := s.Resolve(la.LaunchID)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("resolve launch plan: %w", err)
	}

	plan.BootPrompt = buildResumePrompt(ck, plan.BootPrompt)

	sessID := uuid.NewString()
	wsRoot := plan.WriteHome
	if wsRoot == "" {
		wsRoot = filepath.Join(config.Expand(s.Catalog.Global.Catalog.Defaults.WorkspaceRoot), plan.ProjectID)
	}
	ws, err := workspace.Create(wsRoot, sessID, plan)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("create workspace: %w", err)
	}

	factory, ok := s.factories[plan.ProviderID]
	if !ok {
		return api.LaunchResult{}, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}
	probe, err := factory(plan)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("build runtime: %w", err)
	}

	row := store.SessionRow{
		ID:             sessID,
		LaunchID:       plan.LaunchID,
		ProjectID:      plan.ProjectID,
		LogicalAgentID: plan.LogicalAgentID,
		ProviderID:     plan.ProviderID,
		ProviderKind:   probe.Kind(),
		Workspace:      ws.Root,
		State:          string(session.StateCreated),
	}
	if err := s.Store.CreateSession(row, plan); err != nil {
		return api.LaunchResult{}, fmt.Errorf("persist session: %w", err)
	}

	l, err := s.LaunchSession(sessID)
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{
		SessionID:      l.SessionID,
		Workspace:      l.Workspace.Root,
		LogPath:        l.Workspace.LogPath,
		ProviderID:     l.Plan.ProviderID,
		ProviderKind:   l.ProviderKind,
		LogicalAgentID: l.Plan.LogicalAgentID,
	}, nil
}

// buildResumePrompt prepends a checkpoint context block to the boot prompt.
// Fields that are empty are omitted to keep the context clean.
func buildResumePrompt(ck *checkpoint.Checkpoint, bootPrompt string) string {
	var b strings.Builder
	b.WriteString("## Resumed from checkpoint ")
	b.WriteString(ck.ID)
	b.WriteString("\n")
	if ck.Status != "" {
		b.WriteString("\n**Status:** ")
		b.WriteString(ck.Status)
		b.WriteString("\n")
	}
	if ck.Summary != "" {
		b.WriteString("\n**Summary:**\n")
		b.WriteString(ck.Summary)
		b.WriteString("\n")
	}
	if ck.PendingWork != "" {
		b.WriteString("\n**Pending work:**\n")
		b.WriteString(ck.PendingWork)
		b.WriteString("\n")
	}
	if ck.KeyDecisions != "" {
		b.WriteString("\n**Key decisions:**\n")
		b.WriteString(ck.KeyDecisions)
		b.WriteString("\n")
	}
	if ck.NextRecommendation != "" {
		b.WriteString("\n**Next recommendation:**\n")
		b.WriteString(ck.NextRecommendation)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n\n")
	b.WriteString(bootPrompt)
	return b.String()
}

// providerHasSessionIDContinuity reports whether the given provider ID
// participates in the CLI session-ID continuity mechanism (preset +
// OnSessionID callback). Adapters in this set use --resume or --session
// flags to maintain conversation continuity across daemon restarts.
func providerHasSessionIDContinuity(providerID string) bool {
	switch providerID {
	case "claude-code", "claude-stream", "opencode":
		return true
	}
	return false
}

// goproviderCLIAdapter maps a catalog adapter name to the corresponding
// go-providers CLIAdapter. Returns nil for unknown names; callers skip
// registration silently (a validation error catches unknown names earlier).
func goproviderCLIAdapter(name string) gop.CLIAdapter {
	switch name {
	case "claude":
		return gop.NewClaudeAdapter()
	case "codex":
		return gop.NewCodexAdapter()
	default:
		return nil
	}
}

func resolveAPIKeyHelperPath() string {
	if override := os.Getenv("MUX_APIKEY_HELPER"); override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			override = abs
		}
		if eval, err := filepath.EvalSymlinks(override); err == nil {
			override = eval
		}
		if isExecutableFile(override) {
			return override
		}
	}
	if exe, err := os.Executable(); err == nil {
		if eval, eerr := filepath.EvalSymlinks(exe); eerr == nil {
			exe = eval
		}
		candidate := filepath.Join(filepath.Dir(exe), "mux-apikey-helper")
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	if path, err := exec.LookPath("mux-apikey-helper"); err == nil && isExecutableFile(path) {
		return path
	}
	return ""
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // G703: operator-controlled override/path lookup is the intended trust boundary here
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// seedLogicalAgents upserts a logical_agents row for every catalog agent.
// Returns the number of rows touched. Idempotent: re-running refreshes
// name/role and updated_at while preserving created_at per the Upsert
// contract. See ADR 0003.
func seedLogicalAgents(db *store.Store, agents map[string]config.Agent) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var n int
	for _, a := range agents {
		la := agent.LogicalAgent{
			ID:   a.ID,
			Name: a.Name,
			Role: strings.Join(a.Roles, ", "),
		}
		if err := db.UpsertLogicalAgent(la, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
