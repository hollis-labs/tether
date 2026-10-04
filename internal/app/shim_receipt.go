//go:build !windows

package app

import (
	"context"
	"errors"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"log"
	"path/filepath"
)

func shimFailureCode(err error) string {
	if err == nil {
		return ""
	}
	var host *shimhost.Failure
	if errors.As(err, &host) {
		return host.Code
	}
	var bridge *shimbridge.Failure
	if errors.As(err, &bridge) {
		return bridge.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "outcome_unknown"
}

// loadShimReceipt always reads the private canonical receipt, including its
// start-time witness. Database PIDs alone never authorize inspection or stop.
func loadShimReceipt(row store.SessionShimRow) (shimhost.Receipt, error) {
	var receipt shimhost.Receipt
	if err := shimhost.ReadPrivateJSON(filepath.Join(filepath.Dir(row.DescriptorPath), "placement.json"), shim.MaxFrame, &receipt); err != nil {
		return receipt, &shimhost.Failure{Code: "outcome_unknown", Message: "canonical placement receipt unavailable"}
	}
	if receipt.Session != row.SessionID || receipt.OperationKey != row.ShimKey || receipt.Generation != row.RuntimeGeneration || receipt.Backend != row.HostBackend || receipt.DescriptorPath != row.DescriptorPath || receipt.SocketPath != row.SocketPath || receipt.UnitName != row.UnitName || row.JournalID != "" && receipt.Journal != row.JournalID {
		return shimhost.Receipt{}, &shimhost.Failure{Code: "identity_mismatch", Message: "canonical placement differs from stored session identity"}
	}
	return receipt, nil
}

type shimStatus struct {
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Key     string `json:"placement_key,omitempty"`
	Backend string `json:"backend,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Socket  string `json:"socket,omitempty"`
}

func (s *Service) shimDiagnostic(id string, r *shimhost.Receipt, state, reason string) {
	status := shimStatus{State: state, Reason: reason}
	if r != nil {
		status.Key = r.OperationKey
		status.Backend = r.Backend
		status.Unit = r.UnitName
		status.Socket = r.SocketPath
	}
	log.Printf("session %q shim state=%s reason=%s key=%q backend=%q unit=%q socket=%q", id, status.State, status.Reason, status.Key, status.Backend, status.Unit, status.Socket)
	publishSessionEvent(s.Bus, id, "", "session.shim_status", status)
}

func (s *Service) retainShim(row store.SessionShimRow, receipt *shimhost.Receipt, reason string) {
	if receipt == nil {
		// Diagnostic identity only; never pass this synthesized value to the host.
		receipt = &shimhost.Receipt{OperationKey: row.ShimKey, Backend: row.HostBackend, UnitName: row.UnitName, SocketPath: row.SocketPath}
	}
	if err := s.MarkSessionDetached(row.SessionID, reason); err != nil {
		log.Printf("session %q: retain detached shim failed: %v", row.SessionID, err)
	}
	s.shimDiagnostic(row.SessionID, receipt, "detached", reason)
}

func (s *Service) persistShim(ctx context.Context, id, runtime, boot string, r shimhost.Receipt) error {
	return s.Store.UpsertSessionShim(ctx, store.SessionShimRow{SessionID: id, ShimKey: r.OperationKey, HostBackend: r.Backend, UnitName: r.UnitName, SocketPath: r.SocketPath, DescriptorPath: r.DescriptorPath, JournalID: r.Journal, Runtime: runtime, RuntimeGeneration: r.Generation, BootGeneration: boot, HostPID: r.HostPID, ShimPID: r.ShimPID, ProviderPID: r.ProviderPID})
}
