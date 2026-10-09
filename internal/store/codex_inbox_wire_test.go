//go:build !windows

package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/shimcodex"
)

func TestCodexInboxWireCanonicalJournalRecoveryAndRefusal(t *testing.T) {
	for _, kind := range []string{"valid", "stale", "source_splice", "cursor_splice", "journal_gap", "journal_crc", "retired", "terminal"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newAccountingFixture(t, false)
			f.receipt.Retired = false
			f.saveReceipt(t)
			if _, err := f.store.db.Exec(`UPDATE sessions SET state='running' WHERE id='s'`); err != nil {
				t.Fatal(err)
			}
			port, err := f.store.CodexProtocolStore(ctx, "s", "p", shimcodex.ProjectionBudget)
			if err != nil {
				t.Fatal(err)
			}
			engine, err := shimcodex.Open(ctx, port, f.state.Binding, f.state.Epoch, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: shimcodex.ProjectionBudget})
			if err != nil {
				t.Fatal(err)
			}
			// A pending input effect remains pending even when native spelling is
			// recovered; this helper cannot turn recovery into RPC settlement.
			op, err := engine.Reserve(ctx, f.state.Epoch, "initialize", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Attempt(ctx, f.state.Epoch, op.ID); err != nil {
				t.Fatal(err)
			}
			wire := []byte("{ \"method\":\"unknown/native\", \"params\":{\"text\":\"<>&\u2028\\u0061\"} }\n")
			// Split a native line across physical frames. The identity's cursor
			// belongs to its final frame, while its offsets cover both frames.
			for _, data := range [][]byte{wire[:11], wire[11:]} {
				event := f.append(t, "shim.output", map[string]string{"stream": "stdout", "encoding": "base64", "data": base64.StdEncoding.EncodeToString(data)})
				if ok, err := engine.AcceptOutput(ctx, f.state.Epoch, event.Cursor, "stdout", data); err != nil || !ok {
					t.Fatal("native durable acceptance", err)
				}
			}
			state := engine.Snapshot()
			type legacyEvent struct {
				Identity string          `json:"identity"`
				Cursor   string          `json:"cursor"`
				Raw      json.RawMessage `json:"raw"`
			}
			old := make([]legacyEvent, len(state.Inbox))
			for i, event := range state.Inbox {
				old[i] = legacyEvent{event.Identity, event.Cursor, event.Raw}
			}
			raw, err := json.Marshal(struct {
				shimcodex.State
				Inbox []legacyEvent `json:"inbox"`
			}{state, old})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.Exec(`UPDATE codex_shim_protocol SET state_json=? WHERE session_id='s'`, string(raw)); err != nil {
				t.Fatal(err)
			}
			state, err = port.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_ = f.journal.Close()
			switch kind {
			case "stale":
				state.Revision++
			case "source_splice":
				state.Inbox[len(state.Inbox)-1].Raw = []byte(`{"method":"other/native"}`)
			case "cursor_splice":
				state.Inbox[len(state.Inbox)-1].Cursor = state.Inbox[0].Cursor
			case "journal_gap":
				if err := os.Rename(filepath.Join(f.root, "j", "00000001.seg"), filepath.Join(f.root, "j", "00000002.seg")); err != nil {
					t.Fatal(err)
				}
			case "journal_crc":
				path := filepath.Join(f.root, "j", "00000001.seg")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				body[len(body)-1] ^= 1
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "retired":
				f.receipt.Retired = true
				f.saveReceipt(t)
			case "terminal":
				if _, err := f.store.db.Exec(`UPDATE sessions SET state='completed' WHERE id='s'`); err != nil {
					t.Fatal(err)
				}
			}
			var before string
			if err := f.store.db.QueryRow(`SELECT state_json FROM codex_shim_protocol WHERE session_id='s'`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			witness, err := port.RecoverInboxWire(ctx, state, f.receipt)
			if kind != "valid" {
				if err == nil || witness != nil {
					t.Fatal("ambiguous legacy history accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				last := witness[len(witness)-1]
				if last.LegacyRawJSON || !bytes.Equal(last.Raw, wire[:len(wire)-1]) {
					t.Fatal("journal spelling not recovered")
				}
				engine, err := shimcodex.Open(ctx, port, state.Binding, state.Epoch+1, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: shimcodex.ProjectionBudget})
				if err != nil {
					t.Fatal(err)
				}
				if err := engine.RestoreInboxWire(ctx, func(ctx context.Context, frozen shimcodex.State) ([]shimcodex.Event, error) {
					return port.RecoverInboxWire(ctx, frozen, f.receipt)
				}); err != nil {
					t.Fatal(err)
				}
				got := engine.Snapshot()
				oldOps, _ := json.Marshal(state.Operations)
				newOps, _ := json.Marshal(got.Operations)
				if got.Cursor != state.Cursor || got.NextID != state.NextID || len(got.Inbox) != len(state.Inbox) || !bytes.Equal(oldOps, newOps) || got.Operations[0].Phase != shimcodex.Attempted || got.Delivery.DeliveredHighWater != state.Delivery.DeliveredHighWater {
					t.Fatal("wire restoration settled another obligation")
				}
				return
			}
			var after string
			if err := f.store.db.QueryRow(`SELECT state_json FROM codex_shim_protocol WHERE session_id='s'`).Scan(&after); err != nil || before != after {
				t.Fatal("refusal changed durable obligations", err)
			}
		})
	}
}
