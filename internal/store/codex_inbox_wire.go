//go:build !windows

package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
)

// RecoverInboxWire reads the original native stdout bytes, not a normalized
// JSON approximation. It cannot write, replay, acknowledge, drain, or issue a
// public delivery/replacement capability. The caller commits the witnessed
// encoding through the ordinary protocol CAS, including its old-write fences.
func (p *CodexProtocolStore) RecoverInboxWire(ctx context.Context, frozen shimcodex.State, receipt shimhost.Receipt) ([]shimcodex.Event, error) {
	if p == nil || p.store == nil || receipt.Retired || receipt.PlacementFailure != "" || !receipt.Attempted || receipt.HostPID <= 0 || receipt.ProviderPID <= 0 || receipt.HostStartTime == 0 || receipt.Session != frozen.Binding.Session || receipt.Instance != frozen.Binding.Instance || receipt.OperationKey != frozen.Binding.Operation || receipt.Generation != frozen.Binding.Generation || receipt.Journal != frozen.Binding.Journal || receipt.Fingerprint != frozen.Binding.Fingerprint || receipt.SubmissionAttemptID != frozen.Binding.Attempt || filepath.Base(receipt.DescriptorPath) != "launch.json" {
		return nil, inboxWireRefuse("legacy_custody_mismatch")
	}
	before, err := p.inboxWireSnapshot(ctx, frozen, receipt)
	if err != nil {
		return nil, err
	}
	history, err := accountingReadJournal(ctx, filepath.Join(filepath.Dir(receipt.DescriptorPath), "j"), frozen.Binding)
	if err != nil {
		return nil, inboxWireRefuse("legacy_journal_unavailable")
	}
	position, err := shimCursorPosition(frozen.Binding.Journal, frozen.Cursor)
	if err != nil || position == 0 || position > uint64(len(history.events)) {
		return nil, inboxWireRefuse("legacy_journal_gap")
	}
	result := make([]shimcodex.Event, len(frozen.Inbox))
	copy(result, frozen.Inbox)
	pending := make(map[string]int)
	for i, event := range frozen.Inbox {
		result[i].Raw = append([]byte(nil), event.Raw...)
		if strings.HasPrefix(event.Identity, frozen.Binding.Journal+":stdout:") {
			if _, exists := pending[event.Identity]; exists {
				return nil, inboxWireRefuse("legacy_wire_mismatch")
			}
			pending[event.Identity] = i
		}
	}
	var offset, start uint64
	var carry []byte
	for _, event := range history.events[:position] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Kind != "shim.output" {
			continue
		}
		var output struct {
			Stream   string `json:"stream"`
			Encoding string `json:"encoding"`
			Data     string `json:"data"`
		}
		if json.Unmarshal(event.Payload, &output) != nil || output.Encoding != "base64" || output.Stream != "stdout" && output.Stream != "stderr" {
			return nil, inboxWireRefuse("legacy_wire_mismatch")
		}
		if output.Stream == "stderr" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(output.Data)
		if err != nil || len(data) > shim.OutputChunk || uint64(len(data)) > ^uint64(0)-offset {
			return nil, inboxWireRefuse("legacy_wire_mismatch")
		}
		offset += uint64(len(data))
		carry = append(carry, data...)
		for {
			i := bytes.IndexByte(carry, '\n')
			if i < 0 {
				break
			}
			if i > shimcodex.MaxLineBytes {
				return nil, inboxWireRefuse("legacy_wire_budget")
			}
			end := start + uint64(i) + 1
			identity := fmt.Sprintf("%s:stdout:%d:%d", frozen.Binding.Journal, start, end)
			if index, ok := pending[identity]; ok {
				old := frozen.Inbox[index]
				wire := carry[:i]
				a, errA := json.Marshal(json.RawMessage(old.Raw))
				b, errB := json.Marshal(json.RawMessage(wire))
				if old.Cursor != event.Cursor || errA != nil || errB != nil || !bytes.Equal(a, b) || !old.LegacyRawJSON && !bytes.Equal(old.Raw, wire) {
					return nil, inboxWireRefuse("legacy_wire_mismatch")
				}
				result[index].Raw = append([]byte(nil), wire...)
				result[index].LegacyRawJSON = false
				delete(pending, identity)
			}
			carry = carry[i+1:]
			start = end
		}
		if len(carry) > shimcodex.MaxLineBytes {
			return nil, inboxWireRefuse("legacy_wire_budget")
		}
	}
	if len(pending) != 0 || offset != frozen.StreamOffset || start != frozen.PartialStart || !bytes.Equal(carry, frozen.Partial) {
		return nil, inboxWireRefuse("legacy_wire_mismatch")
	}
	after, err := p.inboxWireSnapshot(ctx, frozen, receipt)
	if err != nil || !bytes.Equal(before, after) {
		return nil, inboxWireRefuse("legacy_snapshot_changed")
	}
	return result, nil
}

func inboxWireRefuse(code string) error { return &shimcodex.Failure{Code: code} }

func (p *CodexProtocolStore) inboxWireSnapshot(ctx context.Context, frozen shimcodex.State, receipt shimhost.Receipt) ([]byte, error) {
	// Load keeps operational tombstone/binding checks; this is not a historical
	// reader and cannot bypass a frozen old session after replacement.
	current, err := p.Load(ctx)
	a, errA := json.Marshal(frozen)
	b, errB := json.Marshal(current)
	if err != nil || errA != nil || errB != nil || !bytes.Equal(a, b) {
		return nil, inboxWireRefuse("legacy_snapshot_changed")
	}
	observed, err := p.store.ReadCodexCandidate(ctx, frozen)
	if err != nil {
		return nil, inboxWireRefuse("legacy_snapshot_changed")
	}
	row := observed.Placement
	// SessionShim.Runtime is the recorded provider ID (for example
	// codex-app-server), not its brand. Require the exact original association
	// and Codex brand from the persisted plan, without consulting the catalog.
	var providerID, providerBrand string
	err = p.store.db.QueryRowContext(ctx, `SELECT json_extract(plan_json,'$.provider_id'),json_extract(plan_json,'$.provider_brand') FROM launch_plans WHERE session_id=? AND length(CAST(plan_json AS BLOB))<=?`, row.SessionID, shimcodex.ProjectionBudget).Scan(&providerID, &providerBrand)
	if err != nil || providerID == "" || providerBrand != "codex" || row.Runtime != providerID || row.HostPID != receipt.HostPID || row.ShimPID != receipt.ShimPID || row.ProviderPID != receipt.ProviderPID || row.HostBackend != receipt.Backend || row.UnitName != receipt.UnitName || row.SocketPath != receipt.SocketPath || row.DescriptorPath != receipt.DescriptorPath {
		return nil, inboxWireRefuse("legacy_custody_mismatch")
	}
	raw, err := accountingReadFile(filepath.Join(filepath.Dir(receipt.DescriptorPath), "placement.json"), 64<<10)
	var canonical shimhost.Receipt
	if err != nil || json.Unmarshal(raw, &canonical) != nil {
		return nil, inboxWireRefuse("legacy_custody_mismatch")
	}
	// Controller inspection refreshes Epoch; provider custody must be exact.
	canonical.Epoch = receipt.Epoch
	if canonical != receipt {
		return nil, inboxWireRefuse("legacy_custody_mismatch")
	}
	return json.Marshal(struct {
		Snapshot      CodexCandidateSnapshot
		Receipt       shimhost.Receipt
		ProviderID    string
		ProviderBrand string
	}{observed, canonical, providerID, providerBrand})
}
