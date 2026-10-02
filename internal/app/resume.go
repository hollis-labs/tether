package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	runtimecheckpoint "github.com/hollis-labs/agentkit/agentruntime/checkpoint"

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
// Returns a not-found-shaped error if no checkpoint exists.
func (s *Service) ResumeLogicalAgent(logicalAgentID string, opts api.ResumeOptions) (api.LaunchResult, error) {
	return s.ResumeLogicalAgentWithContext(context.Background(), logicalAgentID, opts)
}
func (s *Service) ResumeLogicalAgentWithContext(ctx context.Context, logicalAgentID string, opts api.ResumeOptions) (api.LaunchResult, error) {
	if opts.IdempotencyKey == "" {
		return s.resumeLogicalAgent(ctx, logicalAgentID, nil)
	}
	// Idempotent resume (CW-20260930-0229). The digest is the request as
	// sent, so a retry after the resumed session has checkpointed replays
	// that session instead of conflicting over a newer parent.
	unlock := s.lockIdempotencyKey(opts.IdempotencyKey)
	defer unlock()
	digest := resumeRequestDigest(logicalAgentID)
	replayed, err := s.replayIfKeyed(opts.IdempotencyKey, store.IdempotencyOpResume, digest)
	if err != nil {
		return api.LaunchResult{}, err
	}
	if replayed != nil {
		return launchResultOf(replayed), nil
	}
	return s.resumeLogicalAgent(ctx, logicalAgentID, &store.SessionIdempotency{
		Key: opts.IdempotencyKey, Operation: store.IdempotencyOpResume, RequestDigest: digest,
	})
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

func (s *Service) resumeLogicalAgent(ctx context.Context, logicalAgentID string, key *store.SessionIdempotency) (api.LaunchResult, error) {
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
	if hint := resumeHintForCheckpoint(ck, plan); hint.CanResumeNatively() {
		plan.ResumeProviderSessionID = hint.ProviderSessionID
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

	factory, ok := s.factories[plan.ProviderID]
	if !ok {
		return api.LaunchResult{}, fmt.Errorf("no runtime for provider %q", plan.ProviderID)
	}
	probe, err := factory(plan)
	if err != nil {
		return api.LaunchResult{}, fmt.Errorf("build runtime: %w", err)
	}
	if err := s.refuseUnprotectable(plan, probe.Kind()); err != nil {
		return api.LaunchResult{}, err
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
		ParentSessionID: resumeParentSessionID(ck),
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
