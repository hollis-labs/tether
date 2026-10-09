//go:build !windows

package app

import (
	"context"
	"errors"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/substrate/harness/shim"
	"path/filepath"
	"time"

	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

const codexProtocolBudget = 64 << 20
const codexRecordLimit = codexProtocolBudget*2 + 8<<20

func codexBinding(r shimhost.Receipt) shimcodex.Binding {
	return shimcodex.Binding{Session: r.Session, Instance: r.Instance, Generation: r.Generation, Operation: r.OperationKey, Journal: r.Journal, Attempt: r.SubmissionAttemptID, Fingerprint: r.Fingerprint}
}

// The versioned discriminator is durable BEFORE Place; an unknown checkpoint
// can never trigger fallback to a fresh raw JSON-RPC allocator after placement.
func (s *Service) initializeCodexShimCheckpoint(ctx context.Context, r shimhost.Receipt) error {
	p, err := s.Store.CodexProtocolStore(ctx, r.Session, r.OperationKey, codexRecordLimit)
	if err != nil {
		return err
	}
	state, err := p.Load(ctx)
	if errors.Is(err, shimcodex.ErrMissing) {
		return p.Commit(ctx, 0, shimcodex.State{Version: shimcodex.Version, Binding: codexBinding(r), NextID: shimcodex.FirstID, Revision: 1})
	}
	if err != nil {
		return err
	}
	if state.Binding == codexBinding(r) {
		return nil
	}
	// Only an untouched pre-placement discriminator may acquire observed
	// attempt, fingerprint and journal. Any possible input/output freezes it.
	pristine := state.Epoch == 0 && state.NextID == shimcodex.FirstID && len(state.Operations) == 0 && len(state.Inbox) == 0 && len(state.Partial) == 0 && len(state.ServerRequests) == 0 && state.InitializeID == 0 && !state.Initialized && state.ThreadID == "" && state.ActiveTurn == "" && state.LastTerminal == "" && state.Cursor == "" && state.StreamOffset == 0 && state.ReplayHighWater == "" && state.ExitCursor == "" && state.Exit == nil
	bound := state.Binding
	if !pristine || bound.Attempt != "" || bound.Journal != "" || bound.Fingerprint != "" || r.SubmissionAttemptID == "" || r.Journal == "" || r.Fingerprint == "" {
		return store.ErrSessionShimConflict
	}
	bound.Attempt, bound.Journal, bound.Fingerprint = r.SubmissionAttemptID, r.Journal, r.Fingerprint
	if bound != codexBinding(r) {
		return store.ErrSessionShimConflict
	}
	previous := state.Revision
	state.Revision++
	state.Binding = bound
	return p.Commit(ctx, previous, state)
}

func (s *Service) codexShimRuntime(ctx context.Context, id, runtime string, r shimhost.Receipt, fresh bool) (*shimcodex.Runtime, error) {
	p, err := s.Store.CodexProtocolStore(ctx, id, r.OperationKey, codexRecordLimit)
	if err != nil {
		return nil, err
	}
	if _, err := p.Load(ctx); err != nil {
		return nil, err
	}
	host, err := s.shimHost()
	if err != nil {
		return nil, err
	}
	return &shimcodex.Runtime{Config: shimcodex.Config{ID: runtime, Receipt: r, Store: p, Fresh: fresh, Limits: shimcodex.Limits{InboxItems: 1024, InboxBytes: codexProtocolBudget}, DeliveryChecker: codexDeliveryCheck{service: s}, Deliver: s.deliverCodexInbox,
		RecoverInbox: func(ctx context.Context, frozen shimcodex.State) ([]shimcodex.Event, error) {
			return p.RecoverInboxWire(ctx, frozen, r)
		},
		Validate: func(ctx context.Context) error {
			row, err := s.Store.SessionShim(ctx, id)
			if err != nil {
				return err
			}
			current, err := loadShimReceipt(row)
			if err != nil {
				return err
			}
			if current.Retired || current.SubmissionAttemptID != r.SubmissionAttemptID || current.OperationKey != r.OperationKey || current.Fingerprint != r.Fingerprint || current.Session != r.Session || current.Instance != r.Instance || current.Generation != r.Generation || current.Journal != r.Journal || current.HostPID != r.HostPID || current.HostStartTime != r.HostStartTime || current.ShimPID != r.ShimPID || current.ProviderPID != r.ProviderPID || current.Backend != r.Backend || current.UnitName != r.UnitName || current.DescriptorPath != r.DescriptorPath || current.SocketPath != r.SocketPath {
				return &shimhost.Failure{Code: "identity_mismatch", Message: "Codex placement changed"}
			}
			stored, err := s.Store.GetSessionContext(ctx, id)
			if err != nil {
				return err
			}
			if session.State(stored.State).Terminal() || stored.State == string(session.StateOrphaned) || s.stops.requested(id) {
				return &shimhost.Failure{Code: "target_offline", Message: "Codex session authority ended"}
			}
			return ctx.Err()
		},
		OnSession: func(value *shimcodex.Session) {
			host.codex.Store(id, value)
			go func() { _, _ = value.Wait(); host.codex.CompareAndDelete(id, value) }()
		},
		OnDetach: func(cause error) {
			if cause != nil {
				s.shimDiagnostic(id, &r, "detached", codexFailureCode(cause))
			}
		},
	}}, nil
}

// A tracked Codex placement with no local handle is retained, never passed to
// the process-local helper that would initialize/create a replacement thread.
func (s *Service) hostedCodexSession(ctx context.Context, id string) (*shimcodex.Session, bool, error) {
	if s.Store == nil {
		return nil, false, nil
	}
	row, err := s.Store.SessionShim(ctx, id)
	if errors.Is(err, store.ErrSessionShimNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	p, err := s.Store.CodexProtocolStore(ctx, id, row.ShimKey, codexRecordLimit)
	if err != nil {
		return nil, true, err
	}
	_, err = p.Load(ctx)
	if errors.Is(err, shimcodex.ErrMissing) {
		return nil, true, &shimhost.Failure{Code: "outcome_unknown", Message: "hosted Codex protocol identity unavailable"}
	}
	if err != nil {
		return nil, true, err
	}
	host, err := s.shimHost()
	if err != nil {
		return nil, true, err
	}
	value, ok := host.codex.Load(id)
	if !ok {
		return nil, true, &shimhost.Failure{Code: "outcome_unknown", Message: "hosted Codex controller disconnected"}
	}
	handle, ok := value.(*shimcodex.Session)
	if !ok {
		return nil, true, store.ErrSessionShimConflict
	}
	return handle, true, nil
}

// shouldFlushSessionOutput separates a controller Wait from actual provider
// terminal evidence. Pending durable output never disappears behind a flush.
// Direct and previously supported Claude behavior is unchanged.
func (s *Service) shouldFlushSessionOutput(ctx context.Context, id string) bool {
	row, err := s.Store.SessionShim(ctx, id)
	if errors.Is(err, store.ErrSessionShimNotFound) {
		return true
	}
	if err != nil {
		return false
	}
	p, err := s.Store.CodexProtocolStore(ctx, id, row.ShimKey, codexRecordLimit)
	if err != nil {
		return false
	}
	state, err := p.Load(ctx)
	if errors.Is(err, shimcodex.ErrMissing) {
		plan, planErr := s.Store.GetLaunchPlan(id)
		return planErr == nil && plan != nil && plan.ProviderBrand == "claude"
	}
	if err != nil {
		return false
	}
	receipt, err := loadShimReceipt(row)
	if err != nil || state.Binding != codexBinding(receipt) {
		return false
	}
	if len(state.Inbox) != 0 || len(state.Partial) != 0 {
		return false
	}
	if state.Exit != nil {
		return s.codexTerminalSettled(ctx, id, receipt, *state.Exit)
	} // journaled terminal; B2 delivery drained
	// Proven local pre-child refusal is the only failed-start exception.
	return receipt.Retired && receipt.PlacementFailure == "placement_failed" && receipt.HostPID == 0 && receipt.ProviderPID == 0 && state.InitializeID == 0 && len(state.Operations) == 0
}

// Reattachment consumes the existing discriminator and protocol ledger. Missing
// or uncertain identity never falls back to a fresh direct Codex initializer.
func (s *Service) reattachCodexShim(ctx context.Context, tracked store.SessionShimRow, observed shimhost.Receipt) error {
	receipt, err := loadShimReceipt(tracked)
	if err != nil {
		return err
	}
	observed.Epoch = receipt.Epoch // Observer-reported controller epoch is not placement identity.
	if receipt != observed {
		return &shimhost.Failure{Code: "identity_mismatch", Message: "Codex inspected placement changed"}
	}
	p, err := s.Store.CodexProtocolStore(ctx, tracked.SessionID, tracked.ShimKey, codexRecordLimit)
	if err != nil {
		return err
	}
	state, err := p.Load(ctx)
	if err != nil {
		return &shimhost.Failure{Code: "outcome_unknown", Message: "Codex protocol checkpoint unavailable"}
	}
	if state.Binding != codexBinding(receipt) {
		return store.ErrSessionShimConflict
	}
	row, err := s.Store.GetSessionContext(ctx, tracked.SessionID)
	if err != nil {
		return err
	}
	plan, err := s.Store.GetLaunchPlan(row.ID)
	if err != nil {
		return err
	}
	if plan.ProviderBrand != "codex" {
		return store.ErrSessionShimConflict
	}
	rt, err := s.codexShimRuntime(ctx, row.ID, plan.ProviderID, receipt, false)
	if err != nil {
		return err
	}
	opts := agentsessions.StartOptions{JsonRpcRequestHook: jsonRPCRequestHook(row.ID), Workdir: plan.EffectiveWorkRoot(), WorkspaceDir: row.Workspace, LogPath: filepath.Join(row.Workspace, "logs", "session.log"), AttachEnabled: true, OnSessionID: func(id string) { _ = s.Store.UpsertSessionProviderMapping(row.ID, "tether", row.ProviderID, id) }}
	output := s.newSessionTurnOutput(*row, plan)
	output.wire(rt, &opts)
	s.turnOutputs.Store(row.ID, output)
	if err = s.Manager.Start(ctx, agentsessions.StartRequest{ID: row.ID, Runtime: rt, Options: opts, SessionMeta: map[string]string{"logical_agent_id": row.LogicalAgentID, "provider_id": row.ProviderID, "launch_id": row.LaunchID, "project_id": row.ProjectID}}); err != nil {
		s.turnOutputs.CompareAndDelete(row.ID, output)
		return err
	}
	s.finalizeSessionOutput(ctx, row.ID, output)
	s.watchSessionBindings(row.ID)
	host, err := s.shimHost()
	if err != nil {
		return err
	}
	value, ok := host.codex.Load(row.ID)
	if !ok {
		return &shimhost.Failure{Code: "outcome_unknown", Message: "Codex controller not registered"}
	}
	handle, ok := value.(*shimcodex.Session)
	if !ok {
		return store.ErrSessionShimConflict
	}
	if err = handle.WaitReadiness(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Manager.Stop(stopCtx, row.ID)
		_, _ = s.Manager.WaitSession(stopCtx, row.ID)
		_ = s.waitShimBinding(stopCtx, row.ID)
		return &shimhost.Failure{Code: codexFailureCode(err), Message: "Codex controller remains non-running"}
	}
	s.shimDiagnostic(row.ID, &receipt, "running", "reattached")
	return nil
}

func codexFailureCode(err error) string {
	var protocol *shimcodex.Failure
	if errors.As(err, &protocol) {
		return protocol.Code
	}
	return shimFailureCode(err)
}

func (s *Service) codexTerminalSettled(ctx context.Context, id string, r shimhost.Receipt, exit shim.Exit) bool {
	p, err := s.Store.CodexProtocolStore(ctx, id, r.OperationKey, codexRecordLimit)
	if err != nil {
		return false
	}
	state, err := p.Load(ctx)
	if err != nil || state.Binding != codexBinding(r) || state.Exit == nil || *state.Exit != exit || len(state.Inbox) != 0 || len(state.Partial) != 0 {
		return false
	}
	for _, op := range state.Operations {
		if op.EffectUnknown || op.Phase == shimcodex.Intent || op.Phase == shimcodex.Attempted || op.Phase == shimcodex.Written {
			return false
		}
	}
	for _, request := range state.ServerRequests {
		if !request.Written {
			return false
		}
	}
	return s.loadCodexDelivery(ctx, state, state.ReplayHighWater, true) == nil
}

// The consumer is private: no API/provider payload installs a loader or mints
// a receipt. The current Store loader is Unsupported until B2 is assigned.
type codexDeliveryLoader interface {
	LoadVerifiedCodexDelivery(context.Context, shimcodex.State, string) (*store.VerifiedCodexDelivery, error)
}
type codexDeliveryCheck struct{ service *Service }

func (c codexDeliveryCheck) CheckDelivery(ctx context.Context, state shimcodex.State, high string) error {
	return c.service.loadCodexDelivery(ctx, state, high, false)
}
func (s *Service) loadCodexDelivery(ctx context.Context, state shimcodex.State, high string, terminal bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	host, err := s.shimHost()
	if err != nil {
		return store.ErrCodexDeliveryUnsupported
	}
	loader := host.codexDelivery
	if loader == nil {
		loader = s.Store
	}
	row, err := s.Store.SessionShim(ctx, state.Binding.Session)
	if err != nil {
		return err
	}
	canonical, err := loadShimReceipt(row)
	if err != nil || codexBinding(canonical) != state.Binding || canonical.Retired {
		return store.ErrCodexDeliveryUnsupported
	}
	p, err := s.Store.CodexProtocolStore(ctx, row.SessionID, row.ShimKey, codexRecordLimit)
	if err != nil {
		return err
	}
	current, err := p.Load(ctx)
	if err != nil || current.Revision != state.Revision {
		return store.ErrCodexDeliveryUnsupported
	}
	frozenCustody := canonical
	proof, err := loader.LoadVerifiedCodexDelivery(ctx, state, high)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	current, err = p.Load(ctx)
	if err != nil || current.Revision != state.Revision {
		return store.ErrCodexDeliveryUnsupported
	}
	row, err = s.Store.SessionShim(ctx, state.Binding.Session)
	if err != nil {
		return err
	}
	canonical, err = loadShimReceipt(row)
	if err != nil || canonical.Retired || canonical != frozenCustody || codexBinding(canonical) != state.Binding {
		return store.ErrCodexDeliveryUnsupported
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return proof.Validate(s.Store, current, high, terminal)
}
