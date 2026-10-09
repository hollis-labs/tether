//go:build !windows

package app

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/agent"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

// reconcileShimContext spares every tracked shim from the legacy process-PID sweep.
// A refusal or unknown outcome retains authority; positive absence revokes it.
func (s *Service) reconcileShimContext(parent context.Context, stale store.StaleSession) bool {
	row, err := s.Store.SessionShim(context.Background(), stale.ID)
	if errors.Is(err, store.ErrSessionShimNotFound) {
		return false
	}
	if err != nil {
		return true
	}
	lookupContext := parent
	lookupCancel := func() {}
	if parent.Err() != nil {
		// Even an exhausted startup budget must distinguish frozen history
		// before recording the existing bounded refusal. This reads only local
		// lineage and cannot inspect, launch, grant or signal a runtime.
		lookupContext, lookupCancel = context.WithTimeout(context.WithoutCancel(parent), time.Second)
	}
	lookupErr := s.currentShimExecution(lookupContext, stale.ID)
	lookupCancel()
	if lookupErr != nil {
		return true
	}
	if _, live := s.Manager.Get(stale.ID); live {
		return true
	}
	if parent.Err() != nil {
		s.retainShim(row, nil, "startup_budget_exhausted")
		return true
	}
	if s.LaunchHost() != HostShim {
		s.retainShim(row, nil, "shim_reconcile_disabled")
		return true
	}
	if s.recoverUnsubmittedShim(parent, row, stale) {
		return true
	}
	receipt, err := loadShimReceipt(row)
	if err != nil {
		s.retainShim(row, nil, shimFailureCode(err))
		return true
	}
	host, err := s.shimHost()
	if err != nil {
		s.retainShim(row, &receipt, shimFailureCode(err))
		return true
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	inspection, err := host.inspect(ctx, receipt)
	if err != nil {
		s.retainShim(row, &receipt, shimFailureCode(err))
		return true
	}
	plan, planErr := s.Store.GetLaunchPlan(row.SessionID)
	if planErr != nil {
		s.retainShim(row, &receipt, "outcome_unknown")
		return true
	}
	if plan.ProviderBrand == "codex" {
		// Codex custody includes its private protocol inbox. A journaled exit
		// or gone host is not permission to discard undelivered obligations.
		if inspection.Gone {
			if plan.TeamMember {
				if err := s.retireGoneCodexTeam(ctx, row, receipt, host); err != nil {
					s.retainShim(row, &receipt, "outcome_unknown")
				} else {
					s.shimDiagnostic(row.SessionID, &receipt, "recovery_pending", "gone_codex_accounted")
				}
				return true
			}
			s.retainShim(row, &receipt, "outcome_unknown")
			return true
		}
		if err = s.reattachCodexShim(ctx, row, inspection.Receipt); err != nil {
			s.retainShim(row, &receipt, shimFailureCode(err))
		}
		return true
	}
	if inspection.Gone {
		if plan.TeamMember {
			if err = s.recoverGoneTeamShim(ctx, row, receipt, host, recordedPIDAbsent); err != nil {
				s.shimDiagnostic(row.SessionID, &receipt, "unavailable", "gone_team_recovery_refused")
			}
			return true
		}
		// Retire the verified placement before orphaning, so provider secrets
		// cannot outlive authority revocation. Stop's Gone branch signals none.
		if err = host.stopProvider(ctx, receipt); err != nil {
			s.retainShim(row, &receipt, shimFailureCode(err))
			return true
		}
		if err = s.MarkSessionOrphaned(row.SessionID, "shim_gone"); err != nil {
			s.retainShim(row, &receipt, "outcome_unknown")
			return true
		}
		s.shimDiagnostic(row.SessionID, &receipt, "orphaned", "shim_gone")
		return true
	}
	if !inspection.Running {
		// The host positively reports its provider exit; this is not absence.
		if err = host.stopProvider(ctx, receipt); err != nil {
			s.retainShim(row, &receipt, shimFailureCode(err))
			return true
		}
		s.completeShimExit(row, receipt, inspection.Exit)
		return true
	}
	if err = s.reattachShim(ctx, row, inspection.Receipt); err != nil {
		s.retainShim(row, &receipt, shimFailureCode(err))
	}
	return true
}

func (s *Service) reattachShim(ctx context.Context, shimRow store.SessionShimRow, receipt shimhost.Receipt) error {
	if err := s.currentShimExecution(ctx, shimRow.SessionID); err != nil {
		return err
	}
	host, err := s.shimHost()
	if err != nil {
		return err
	}
	planForRuntime, err := s.Store.GetLaunchPlan(shimRow.SessionID)
	if err != nil {
		return err
	}
	if planForRuntime.ProviderBrand == "codex" {
		return s.reattachCodexShim(ctx, shimRow, receipt)
	}
	// Re-read before handing the controller to the bridge, after Inspect.
	receipt, err = loadShimReceipt(shimRow)
	if err != nil {
		return err
	}
	checkpoint, err := shimbridge.ReadCheckpoint(filepath.Join(filepath.Dir(receipt.DescriptorPath), "bridge.json"))
	if err != nil {
		return &shimhost.Failure{Code: "outcome_unknown", Message: "bridge checkpoint unavailable"}
	}
	if err = initializeShimCheckpoint(receipt); err != nil {
		return err
	}
	if checkpoint.Journal == "" {
		checkpoint.Journal = receipt.Journal
	}
	if checkpoint.Journal != receipt.Journal || checkpoint.Session != receipt.Session || checkpoint.Instance != receipt.Instance || checkpoint.Generation != receipt.Generation {
		return &shimhost.Failure{Code: "journal_mismatch", Message: "bridge checkpoint differs from placement"}
	}
	row, err := s.Store.GetSession(shimRow.SessionID)
	if err != nil {
		return err
	}
	plan, err := s.Store.GetLaunchPlan(row.ID)
	if err != nil {
		return err
	}
	rt, err := s.shimBridgeRuntime(row.ID, plan.ProviderID, host.bridge, agentsessions.Capabilities{StreamingStdio: true, ProviderSessionID: true, BinaryRequired: true})
	if err != nil {
		return err
	}
	var nativeID string
	if mapping, e := s.Store.GetSessionProviderMapping(row.ID, "tether", row.ProviderID); e == nil {
		nativeID = mapping.NativeSessionID.String
	}
	opts := shimBridgeOptions(agentsessions.StartOptions{Workdir: plan.EffectiveWorkRoot(), WorkspaceDir: row.Workspace, LogPath: filepath.Join(row.Workspace, "logs", "session.log"), SessionIDPreset: nativeID, AttachEnabled: true, OnSessionID: func(id string) { _ = s.Store.UpsertSessionProviderMapping(row.ID, "tether", row.ProviderID, id) }}, host.bridge, receipt, true)
	if opts.Workdir == "" {
		opts.Workdir = plan.RepoRoot
	}
	output := s.newSessionTurnOutput(*row, plan)
	output.wire(rt, &opts)
	s.turnOutputs.Store(row.ID, output)
	if err = s.Manager.Start(context.WithoutCancel(ctx), agentsessions.StartRequest{ID: row.ID, Runtime: rt, Options: opts, SessionMeta: map[string]string{"logical_agent_id": row.LogicalAgentID, "provider_id": row.ProviderID, "launch_id": row.LaunchID, "project_id": row.ProjectID}}); err != nil {
		s.turnOutputs.Delete(row.ID)
		return err
	}
	s.finalizeSessionOutput(ctx, row.ID, output)
	s.watchSessionBindings(row.ID)
	if err = host.waitHandshake(ctx, receipt, checkpoint.ControllerEpoch); err != nil {
		// Close only the unsettled bridge. Its provider and canonical placement
		// remain available for a later reconciliation attempt.
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Manager.Stop(stopCtx, row.ID)
		_, _ = s.Manager.WaitSession(stopCtx, row.ID)
		_ = s.waitShimBinding(stopCtx, row.ID)
		return err
	}
	s.shimDiagnostic(row.ID, &receipt, "running", "reattached")
	return nil
}

func (s *Service) stopShimSession(id string) (bool, error) {
	row, err := s.Store.SessionShim(context.Background(), id)
	if errors.Is(err, store.ErrSessionShimNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if err := s.currentShimExecution(context.Background(), id); err != nil {
		return true, err
	}
	sessionRow, err := s.Store.GetSession(id)
	if err != nil {
		return true, err
	}
	if session.State(sessionRow.State).Terminal() || sessionRow.State == string(session.StateOrphaned) {
		return true, agentsessions.ErrSessionNotRunning
	}
	receipt, err := loadShimReceipt(row)
	if err != nil {
		s.shimDiagnostic(id, nil, "retained", shimFailureCode(err))
		return true, err
	}
	host, err := s.shimHost()
	if err != nil {
		return true, err
	}
	if !receipt.Retired {
		sessionRow, err := s.Store.GetSession(id)
		if err != nil {
			return true, err
		}
		if strings.TrimSpace(sessionRow.LogicalAgentID) != "" {
			policy, err := s.Store.GetLogicalAgentPolicy(sessionRow.LogicalAgentID)
			if err != nil {
				return true, err
			}
			if policy.CheckpointPolicy == agent.CheckpointPolicyOnStop {
				if err := s.CreatePolicyCheckpoint(sessionRow, policy); err != nil {
					return true, err
				}
			}
		}
	}
	s.stops.mark(id)
	defer s.stops.clear(id)
	plan, err := s.Store.GetLaunchPlan(id)
	if err != nil {
		return true, err
	}
	durations, err := plan.Lifecycle.Durations()
	if err != nil {
		return true, err
	}
	if host.stop != nil {
		err = host.stopProvider(context.Background(), receipt)
	} else {
		err = host.provider.StopWithPolicy(context.Background(), receipt, shimhost.StopPolicy{
			RequestGrace: durations.RequestGrace, TerminateGrace: durations.TerminateGrace, KillGrace: durations.KillGrace,
			BeforeStage: func(ctx context.Context, stage string) error {
				reason := s.stops.reason(id)
				if reason == "" {
					reason = "user_stop"
				}
				return s.reaperEvent(ctx, sessionRow, reason, stage, "", nil)
			},
		})
	}
	if err != nil {
		s.shimDiagnostic(id, &receipt, "retained", shimFailureCode(err))
		return true, err
	}
	// Only successful host teardown authorizes terminal state or bridge closure.
	if _, live := s.Manager.Get(id); live {
		_ = s.Manager.Stop(context.Background(), id)
		_, _ = s.Manager.WaitSession(context.Background(), id)
	}
	change, changed, err := s.Store.CompleteSessionShim(context.Background(), id, string(session.StateKilled), 0)
	if err != nil {
		return true, err
	}
	if changed {
		publishSessionEvent(s.Bus, id, change.LogicalAgentID, events.KindSessionStateChanged, sessionStateChangedPayload{From: change.From, To: change.To, ExitCode: new(int), Reason: s.stops.reason(id)})
	}
	if err = s.waitShimBinding(context.Background(), id); err != nil {
		return true, err
	}
	if s.Registry != nil {
		_, _ = s.Registry.RevokeSessionBindings(context.Background(), id)
	}
	if cleanup, ok := host.cleanup.LoadAndDelete(id); ok {
		cleanup.(func())()
	}
	s.shimDiagnostic(id, &receipt, "retired", "user_stop")
	return true, nil
}

// An absent descriptor and receipt under the placement lock prove that exec
// was never submitted: Place persists both before starting the host.
func (s *Service) recoverUnsubmittedShim(parent context.Context, row store.SessionShimRow, stale store.StaleSession) bool {
	if stale.State != string(session.StateLaunching) || row.HostPID != 0 || row.ProviderPID != 0 {
		return false
	}
	host, err := s.shimHost()
	if err != nil {
		return false
	}
	expected := host.provider.PlacementIdentity(row.ShimKey, shim.Launch{Session: row.SessionID, Instance: host.instance, Generation: row.RuntimeGeneration})
	if row.DescriptorPath != expected.DescriptorPath || row.SocketPath != expected.SocketPath {
		return false
	}
	dir := filepath.Dir(row.DescriptorPath)
	if err := shimhost.PrivateDir(dir); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	lock, err := shimhost.LockWait(ctx, filepath.Join(dir, "placement.lock"), 2*time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = lock.Close() }()
	for _, path := range []string{row.DescriptorPath, filepath.Join(dir, "placement.json")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	if err := s.Store.RemoveUnstartedSessionShim(ctx, row.SessionID, row.ShimKey); err != nil {
		return false
	}
	exit := -1
	if err := s.Store.UpdateSessionState(row.SessionID, string(session.StateFailed), 0, &exit); err != nil {
		return true
	}
	s.shimDiagnostic(row.SessionID, nil, "failed", "shim_not_submitted")
	return true
}
