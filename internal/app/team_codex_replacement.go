//go:build !windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/workspace"
)

// Startup may retire only positively absent, fully accounted custody. It does
// not move enrollment or launch; the post-listener team receipt owner performs
// that transition under the original lease and session/actor gates.
func (s *Service) retireGoneCodexTeam(ctx context.Context, tracked store.SessionShimRow, receipt shimhost.Receipt, host *shimHosting) error {
	if s.currentShimExecution(ctx, tracked.SessionID) != nil || !recordedPIDAbsent(receipt.HostPID) || !recordedPIDAbsent(receipt.ProviderPID) {
		return store.ErrSessionReplacementUnavailable
	}
	fence, err := s.Store.GoneTeamShimRecoveryFence(ctx, tracked)
	if err != nil {
		return err
	}
	state, err := s.codexReplacementState(ctx, tracked, receipt)
	if err != nil {
		return err
	}
	if _, err := s.Store.AccountCodexReplacement(ctx, state, receipt); err != nil {
		return err
	}
	if !receipt.Retired {
		// The canonical host Gone branch signals no process. Unknown control
		// effects or incomplete history have already refused above.
		if err := host.stopProvider(ctx, receipt); err != nil {
			return err
		}
	}
	current, err := loadShimReceipt(tracked)
	expected := receipt
	expected.Retired = true
	if err != nil || current != expected || !recordedPIDAbsent(current.HostPID) || !recordedPIDAbsent(current.ProviderPID) {
		return store.ErrSessionReplacementUnavailable
	}
	after, err := s.Store.GoneTeamShimRecoveryFence(ctx, tracked)
	if err != nil || after != fence {
		return store.ErrSessionReplacementUnavailable
	}
	_, err = s.Store.AccountCodexReplacement(ctx, state, current)
	return err
}

func (s *Service) codexReplacementState(ctx context.Context, tracked store.SessionShimRow, receipt shimhost.Receipt) (shimcodex.State, error) {
	p, err := s.Store.CodexProtocolStore(ctx, tracked.SessionID, tracked.ShimKey, codexRecordLimit)
	if err != nil {
		return shimcodex.State{}, err
	}
	state, err := p.Load(ctx)
	if err != nil || state.Binding != codexBinding(receipt) {
		return shimcodex.State{}, store.ErrSessionReplacementUnavailable
	}
	return state, nil
}

// The caller holds the original canonical session gate and retained receipt
// lease. Surviving/uncertain custody never enters this replacement path.
func (s *Service) recoverRetainedCodexTeam(ctx context.Context, key string, row *store.SessionRow, tracked store.SessionShimRow) error {
	if _, live := s.Manager.Get(row.ID); live {
		return store.ErrSessionReplacementUnavailable
	}
	plan, err := s.Store.GetLaunchPlan(row.ID)
	if err != nil || !plan.TeamMember || plan.ProviderBrand != "codex" {
		return store.ErrSessionReplacementUnavailable
	}
	canonical, err := loadShimReceipt(tracked)
	if err != nil || !canonical.Retired || !recordedPIDAbsent(canonical.HostPID) || !recordedPIDAbsent(canonical.ProviderPID) {
		return store.ErrSessionReplacementUnavailable
	}
	state, err := s.codexReplacementState(ctx, tracked, canonical)
	if err != nil {
		return err
	}
	proof, err := s.Store.AccountCodexReplacement(ctx, state, canonical)
	if err != nil {
		return err
	}
	var actor, sourcePlanJSON string
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT json_extract(request,'$.Actor') FROM team_port_intents WHERE port_kind='session' AND intent_key=? AND ended='' AND state='done' AND CAST(payload AS TEXT)=json_quote(?)`, key, row.ID).Scan(&actor); err != nil || actor == "" {
		return store.ErrSessionReplacementUnavailable
	}
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT plan_json FROM launch_plans WHERE session_id=?`, row.ID).Scan(&sourcePlanJSON); err != nil {
		return err
	}
	plan.ResumeSourceSessionID, plan.RecoveryActorURI = row.ID, actor
	mapping, err := s.Store.GetSessionProviderMapping(row.ID, "tether", plan.ProviderID)
	if err != nil && !errors.Is(err, store.ErrProviderMappingNotFound) {
		return err
	}
	plan.ResumeProviderSessionID = mapping.NativeSessionID.String
	checkpoint, err := s.Store.GetLatestCheckpointForAgent(plan.LogicalAgentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	pack := s.recoveryContext(ctx, plan, checkpoint)
	plan.RecoveryPrompt = pack.prompt(buildResumePrompt(checkpoint, plan.BootPrompt))
	unlock, err := s.lockSessionLaunch(ctx, "actor:"+actor)
	if err != nil {
		return err
	}
	defer unlock()
	if !filepath.IsAbs(row.Workspace) {
		return store.ErrSessionReplacementUnavailable
	}
	id := uuid.NewString()
	destination := filepath.Join(filepath.Dir(row.Workspace), id)
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return store.ErrSessionReplacementUnavailable
	}
	ws, err := workspace.Create(filepath.Dir(row.Workspace), id, plan)
	if err != nil {
		return err
	}
	ts, err := teamstore.New(s.Store.DB(), teamstore.Options{})
	if err != nil {
		return err
	}
	err = ts.WithTransaction(ctx, func(conn *sql.Conn) error {
		_, err := s.Store.PrepareCodexTeamReplacementTx(ctx, conn, store.TeamReplacementInput{Source: *row, SourcePlanJSON: sourcePlanJSON, DestinationID: id, Workspace: ws.Root, IntentKey: key, Plan: plan, CredentialMode: s.Catalog.Global.Identity.EffectiveMode()}, proof, canonical)
		return err
	})
	if err != nil {
		return err
	}
	return s.launchRetainedTeamReplacement(ctx, key, id)
}
