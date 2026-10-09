//go:build !windows

package shimcodex

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// InboxWireReader is trusted host composition, never a provider/client RPC.
// It witnesses legacy native bytes from the canonical preserved journal. The
// Engine still commits through its ordinary conditional protocol store.
type InboxWireReader func(context.Context, State) ([]Event, error)

func needsInboxWire(s State) bool {
	for _, event := range s.Inbox {
		if event.LegacyRawJSON && strings.HasPrefix(event.Identity, s.Binding.Journal+":stdout:") {
			return true
		}
	}
	return false
}

// RestoreInboxWire changes only the encoding of retained source bytes. It
// cannot acknowledge/drain an inbox, advance replay, settle operations, change
// native identity, or mint delivery proof. Any uncertain CAS poisons normally.
func (e *Engine) RestoreInboxWire(ctx context.Context, read InboxWireReader) error {
	frozen := e.Snapshot()
	if !needsInboxWire(frozen) {
		return nil
	}
	if read == nil {
		return fail("legacy_bytes_pending")
	}
	witness, err := read(ctx, clone(frozen))
	if err != nil {
		return err
	}
	if len(witness) != len(frozen.Inbox) {
		return fail("legacy_wire_mismatch")
	}
	for i, old := range frozen.Inbox {
		next := witness[i]
		if old.Identity != next.Identity || old.Cursor != next.Cursor {
			return fail("legacy_wire_mismatch")
		}
		if old.LegacyRawJSON && strings.HasPrefix(old.Identity, frozen.Binding.Journal+":stdout:") {
			// JSON Marshal reproduces the old storage normalization, including
			// whitespace compaction and HTML escapes. Equality alone is not a
			// byte witness: the trusted reader must also verify journal spans.
			a, errA := json.Marshal(json.RawMessage(old.Raw))
			b, errB := json.Marshal(json.RawMessage(next.Raw))
			if next.LegacyRawJSON || errA != nil || errB != nil || !bytes.Equal(a, b) {
				return fail("legacy_wire_mismatch")
			}
		} else if old.LegacyRawJSON != next.LegacyRawJSON || !bytes.Equal(old.Raw, next.Raw) {
			return fail("legacy_wire_mismatch")
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Revision != frozen.Revision || e.state.Binding != frozen.Binding || e.state.Epoch != frozen.Epoch {
		return fail("checkpoint_conflict")
	}
	return e.commitLocked(ctx, func(next *State) error {
		next.Inbox = clone(State{Inbox: witness}).Inbox
		return nil
	})
}
