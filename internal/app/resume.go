package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	runtimecheckpoint "github.com/hollis-labs/substrate/harness/adapters/checkpoint"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

// ResumeLogicalAgent starts a new session for the given logical agent
// using its most recent checkpoint as boot context. The launch profile
// from the agent's most recent previous session (logical_agents.launch_id)
// is reused.
//
// Returns a conflict error if the agent has never launched (no launch_id).
// A checkpoint is optional when a previous canonical session exists.
func (s *Service) ResumeLogicalAgent(logicalAgentID string, opts api.ResumeOptions) (api.LaunchResult, error) {
	return s.ResumeLogicalAgentWithContext(context.Background(), logicalAgentID, opts)
}
func (s *Service) ResumeLogicalAgentWithContext(ctx context.Context, logicalAgentID string, opts api.ResumeOptions) (api.LaunchResult, error) {
	gate, _ := s.resumeLocks.LoadOrStore(logicalAgentID, make(chan struct{}, 1))
	select {
	case gate.(chan struct{}) <- struct{}{}:
		defer func() { <-gate.(chan struct{}) }()
	case <-ctx.Done():
		return api.LaunchResult{}, ctx.Err()
	}
	if opts.NativeOnly && opts.SourceSessionID == "" || !opts.NativeOnly && (opts.SourceSessionID != "" || opts.ResumeWorkRoot != "") {
		return api.LaunchResult{}, nativeOnlyError("exact source and native-only mode required")
	}
	if opts.IdempotencyKey == "" {
		return s.resumeLogicalAgent(ctx, logicalAgentID, nil, opts)
	}
	// Idempotent resume (CW-20260930-0229). The digest is the request as
	// sent, so a retry after the resumed session has checkpointed replays
	// that session instead of conflicting over a newer parent.
	unlock := s.lockIdempotencyKey(opts.IdempotencyKey)
	defer unlock()
	digest := resumeRequestDigest(logicalAgentID)
	if opts.NativeOnly {
		digest = requestDigest(struct {
			Agent, Source, WorkRoot string
			NativeOnly              bool
		}{logicalAgentID, opts.SourceSessionID, opts.ResumeWorkRoot, true})
	}
	replayed, err := s.replayIfKeyed(opts.IdempotencyKey, store.IdempotencyOpResume, digest)
	if err != nil {
		return api.LaunchResult{}, err
	}
	if replayed != nil {
		return launchResultOf(replayed), nil
	}
	return s.resumeLogicalAgent(ctx, logicalAgentID, &store.SessionIdempotency{
		Key: opts.IdempotencyKey, Operation: store.IdempotencyOpResume, RequestDigest: digest,
	}, opts)
}

func launchResultOf(l *Launched) api.LaunchResult {
	return api.LaunchResult{
		SessionID:      l.SessionID,
		Workspace:      l.Workspace.Root,
		LogPath:        l.Workspace.LogPath,
		ProviderID:     l.Plan.ProviderID,
		ProviderKind:   l.ProviderKind,
		LogicalAgentID: l.Plan.LogicalAgentID,
		Replayed:       l.Replayed,
	}
}

func (s *Service) resumeLogicalAgent(ctx context.Context, logicalAgentID string, key *store.SessionIdempotency, opts api.ResumeOptions) (api.LaunchResult, error) {
	la, err := s.Store.GetLogicalAgent(logicalAgentID)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("get logical agent: %w", err)
	}
	if la.LaunchID == "" {
		return api.LaunchResult{}, fmt.Errorf("agent %q has never launched a session; cannot resume", logicalAgentID)
	}

	detachedID, err := s.Store.DetachedSessionIDForAgent(ctx, logicalAgentID)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("check detached agent sessions: %w", err)
	}
	if detachedID != "" {
		return api.LaunchResult{}, fmt.Errorf("%w (session=%s)", session.ErrDetached, detachedID)
	}

	var ck *checkpoint.Checkpoint
	if !opts.NativeOnly {
		ck, err = s.Store.GetLatestCheckpointForAgent(logicalAgentID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return api.LaunchResult{}, fmt.Errorf("get latest checkpoint: %w", err)
		}
	}
	parent, err := s.Store.LatestSessionForAgent(ctx, logicalAgentID)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("get resume source: %w", err)
	}
	if opts.NativeOnly && (parent == nil || parent.ID != opts.SourceSessionID) {
		return api.LaunchResult{}, nativeOnlyError("canonical source changed or missing")
	}
	if parent == nil && ck == nil {
		return api.LaunchResult{}, fmt.Errorf("%w: agent %q has no previous session or checkpoint", store.ErrSessionNotFound, logicalAgentID)
	}
	live, err := s.Store.AgentHasLiveSession(ctx, logicalAgentID)
	if err != nil {
		return api.LaunchResult{}, err
	}
	if live {
		return api.LaunchResult{}, fmt.Errorf("%w: agent %q already has a live session", session.ErrRecoveryConflict, logicalAgentID)
	}
	// Older checkpoints remain recovery context, but never override the native
	// conversation or workspace of a newer canonical session.
	sourceID := ""
	if parent != nil {
		sourceID = parent.ID
	} else if ck != nil {
		sourceID = ck.SourceSessionID
	}

	if sourceID != "" {
		parent, err = s.Store.GetSession(sourceID)
		if err != nil && !errors.Is(err, store.ErrSessionNotFound) {
			return api.LaunchResult{}, fmt.Errorf("get resume source session: %w", err)
		}
		if parent != nil && session.State(parent.State) == session.StateDetached {
			return api.LaunchResult{}, fmt.Errorf("%w (session=%s)", session.ErrDetached, parent.ID)
		}
		// Terminal state alone does not retire a shim's process or protocol
		// custody. Its owning reattachment/retirement path must settle that
		// receipt before logical resume may allocate another session.
		if _, custodyErr := s.Store.SessionShim(ctx, sourceID); custodyErr == nil {
			return api.LaunchResult{}, fmt.Errorf("%w: resume source retains tracked shim custody (session=%s)", session.ErrRecoveryConflict, sourceID)
		} else if !errors.Is(custodyErr, store.ErrSessionShimNotFound) {
			return api.LaunchResult{}, fmt.Errorf("read resume source custody: %w", custodyErr)
		}
	}

	// Preserve the original opt-in before resolving the current catalog route.
	input := launch.Input{LaunchID: la.LaunchID, CatalogRoot: s.CatalogRoot}
	if sourceID != "" {
		input.RouteOverrideSet = true
		input.RouteOverride, err = s.Store.SessionRoute(ctx, sourceID)
		if err != nil {
			return api.LaunchResult{}, fmt.Errorf("read resumed session route: %w", err)
		}
	}
	cat, err := s.launchCatalog(la.LaunchID)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("read launch catalog: %w", err)
	}
	plan, err := launch.Resolve(cat, input)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("resolve launch plan: %w", err)
	}

	plan.NativeResumeOnly = opts.NativeOnly
	if !opts.NativeOnly {
		plan.BootPrompt = buildResumePrompt(ck, plan.BootPrompt)
	}
	plan.ResumeSourceSessionID = sourceID
	if parent != nil {
		if parent.LogicalAgentID != logicalAgentID {
			return api.LaunchResult{}, fmt.Errorf("%w: resume source belongs to another logical agent", session.ErrRecoveryConflict)
		}
		prior, err := s.Store.GetLaunchPlan(sourceID)
		if err != nil {
			return api.LaunchResult{}, fmt.Errorf("read resume workspace: %w", err)
		}
		if prior.TeamMember {
			return api.LaunchResult{}, fmt.Errorf("%w: enrolled team sessions require recovery of their retained session identity", session.ErrRecoveryConflict)
		}
		plan.WorkRoot = prior.EffectiveWorkRoot()
		if parent.ProviderID == plan.ProviderID && prior.ProviderBrand == plan.ProviderBrand {
			plan.NativeStateRoot = prior.NativeStateRoot
			mapping, mapErr := s.Store.GetSessionProviderMapping(sourceID, "tether", plan.ProviderID)
			if mapErr == nil {
				plan.ResumeProviderSessionID = mapping.NativeSessionID.String
			} else if !errors.Is(mapErr, store.ErrProviderMappingNotFound) {
				return api.LaunchResult{}, fmt.Errorf("read native resume mapping: %w", mapErr)
			} else if prior.ResumeSourceSessionID != "" && prior.ResumeProviderSessionID != "" {
				// Failed admission (for example unavailable credentials) did not
				// bind a new native ID. Its uncleared requested hint remains intent;
				// a mapping tombstone above always overrides it after native loss.
				plan.ResumeProviderSessionID = prior.ResumeProviderSessionID
			} else if ck != nil && ck.SourceSessionID == sourceID {
				plan.ResumeProviderSessionID = resumeHintForCheckpoint(ck, plan).ProviderSessionID
			}
		}
	}
	if opts.NativeOnly {
		if opts.ResumeWorkRoot != "" {
			plan.NativeResumeWorkRoot, plan.WorkRoot = opts.ResumeWorkRoot, opts.ResumeWorkRoot
		}
		if err := s.validateNativeOnlySource(ctx, parent, plan); err != nil {
			return api.LaunchResult{}, err
		}
		if err := s.nativeOnlyLaunchAuthority(ctx, plan); err != nil {
			return api.LaunchResult{}, err
		}
		plan.BootPrompt, plan.RecoveryPrompt, plan.RecoveryCursors = "", "", nil
	} else {
		pack := s.recoveryContext(ctx, plan, ck)
		plan.BootPrompt = pack.prompt(plan.BootPrompt)
	}

	factory, ok := s.factories[plan.ProviderID]
	if !ok {
		return api.LaunchResult{}, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}
	probe, err := factory(plan)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("build runtime: %w", err)
	}
	if opts.NativeOnly && !probe.Caps().JsonRpcStdio {
		return api.LaunchResult{}, nativeOnlyError("provider has no existing-thread RPC path")
	}
	if err := s.refuseUnprotectable(plan, probe.Kind()); err != nil {
		return api.LaunchResult{}, err
	}

	sessID := uuid.NewString()
	wsRoot := plan.WriteHome
	if wsRoot == "" {
		// Explicit catalog workspace_root wins; cat.Paths supplies the
		// go-apppaths fallback only when global.yaml omits the key.
		wsRoot = filepath.Join(config.ResolveWorkspaceRoot(s.Catalog.Global.Catalog.Defaults, s.Catalog.Paths), plan.ProjectID)
	}
	ws, err := workspace.Create(wsRoot, sessID, plan)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("create workspace: %w", err)
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
		// Canonical-session lineage (T02, messaging vNext). Before this,
		// Tether had no session->session lineage at all: the only backward
		// pointer was checkpoint.SourceSessionID (which session PRODUCED
		// the checkpoint being resumed from). ParentSessionID makes that
		// lineage visible on the new session row itself, without changing
		// the existing checkpoint-driven --resume decision above.
		Intent:          "resume",
		ParentSessionID: sql.NullString{String: sourceID, Valid: sourceID != ""},
	}
	if key != nil {
		// Audit only: the parent the resolver chose, outside the digest.
		key.ResolvedParentSessionID = row.ParentSessionID
	}
	if err := s.Store.CreateSessionKeyed(row, plan, key); err != nil {
		return api.LaunchResult{}, fmt.Errorf("persist session: %w", err)
	}

	l, err := s.LaunchSessionWithContext(ctx, sessID)
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

// resumeParentSessionID resolves the new session's lineage pointer (T02,
// messaging vNext) from the checkpoint being resumed. Empty when the
// checkpoint doesn't know its source session -- a pre-T02 checkpoint row,
// or a checkpoint that was never associated with a producing session.
func resumeParentSessionID(ck *checkpoint.Checkpoint) sql.NullString {
	if ck == nil || ck.SourceSessionID == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: ck.SourceSessionID, Valid: true}
}

func resumeHintForCheckpoint(ck *checkpoint.Checkpoint, plan *launch.Plan) runtimecheckpoint.ResumeHint {
	hint := runtimecheckpoint.ResumeHint{
		Support:           runtimecheckpoint.ResumeFreshBoot,
		FallbackFreshBoot: true,
	}
	if plan != nil {
		hint.Provider = plan.ProviderBrand
		hint.Runtime = plan.RuntimeKind
	}
	if ck == nil || ck.ProviderHintsJSON == "" {
		return hint
	}
	var payload struct {
		ProviderSessionID string `json:"provider_session_id"`
		SessionID         string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(ck.ProviderHintsJSON), &payload); err == nil {
		hint.ProviderSessionID = payload.ProviderSessionID
		if hint.ProviderSessionID == "" {
			hint.ProviderSessionID = payload.SessionID
		}
	}
	if hint.ProviderSessionID != "" {
		hint.Support = runtimecheckpoint.ResumeNative
	}
	return hint
}

// buildResumePrompt prepends a checkpoint context block to the boot prompt.
// Fields that are empty are omitted to keep the context clean.
func buildResumePrompt(ck *checkpoint.Checkpoint, bootPrompt string) string {
	if ck == nil {
		return "## Recovery\nThe previous process ended without a checkpoint. Inspect durable work before continuing; do not replay an interrupted turn.\n\n" + bootPrompt
	}
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
