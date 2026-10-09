//go:build !windows

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/turnoutput"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
)

type accountingFixture struct {
	store   *Store
	state   shimcodex.State
	receipt shimhost.Receipt
	journal *shim.Journal
	root    string
	routed  bool
}

func newAccountingFixture(t *testing.T, reply bool, routed ...bool) accountingFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan := &launch.Plan{ProviderBrand: "codex"}
	selected := len(routed) != 0 && routed[0]
	if selected {
		plan.Route = &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}
	}
	if err = db.CreateSession(SessionRow{ID: "s", State: "running"}, plan); err != nil {
		t.Fatal(err)
	}
	j, err := shim.OpenJournal(filepath.Join(root, "j"), "s", 1, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	f := accountingFixture{store: db, journal: j, root: root, routed: selected}
	first := f.append(t, "shim.launch_intent", map[string]string{"instance": "i"})
	journal, _, _ := strings.Cut(first.Cursor, ":")
	f.receipt = shimhost.Receipt{Session: "s", Instance: "i", OperationKey: "p", SubmissionAttemptID: "a", Generation: 1, Journal: journal, Fingerprint: "f", DescriptorPath: filepath.Join(root, "launch.json"), SocketPath: filepath.Join(root, "c", "control.sock"), Backend: "detached", Attempted: true, HostPID: 100, ShimPID: 100, ProviderPID: 101, HostStartTime: 123}
	f.state = shimcodex.State{Version: shimcodex.Version, Binding: shimcodex.Binding{Session: "s", Instance: "i", Operation: "p", Attempt: "a", Fingerprint: "f", Generation: 1, Journal: journal}, Revision: 1, Epoch: 1, NextID: shimcodex.FirstID}
	f.metadata(first)
	f.metadata(f.append(t, "shim.started", map[string]int{"pid": 101}))
	if reply {
		results := []string{`{}`, ``, `{"thread":{"id":"thread"}}`, `{"turn":{"id":"turn"}}`}
		for i, method := range []string{"initialize", "initialized", "thread/start", "turn/start"} {
			id := shimcodex.FirstID + uint64(i)
			f.state.Operations = append(f.state.Operations, shimcodex.Operation{ID: id, Method: method, Params: json.RawMessage(`{}`), Result: json.RawMessage(results[i]), Notification: i == 1, Phase: shimcodex.Answered})
			key := "codex-" + strconv.FormatUint(id, 10)
			intent := f.append(t, "shim.inject_intent", map[string]string{"key": key, "fingerprint": "input-fingerprint", "mode": "input", "delivery": "immediate"})
			f.metadata(intent)
			f.metadata(f.append(t, "shim.inject_outcome", map[string]any{"key": key, "receipt": map[string]any{"fingerprint": "input-fingerprint", "code": "bytes_written", "cursor": intent.Cursor, "bytes": 30}}))
		}
		f.state.NextID += 4
		f.state.InitializeID = shimcodex.FirstID
		f.state.Initialized = true
		f.state.ThreadID = "thread"
		f.state.LastTerminal = "turn"
		lines := []string{
			fmt.Sprintf(`{"id":%d,"result":{}}`, shimcodex.FirstID),
			fmt.Sprintf(`{"id":%d,"result":{"thread":{"id":"thread"}}}`, shimcodex.FirstID+2),
			fmt.Sprintf(`{"id":%d,"result":{"turn":{"id":"turn"}}}`, shimcodex.FirstID+3),
			`{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn","status":"inProgress"}}}`,
			`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"id":"message","type":"agentMessage","text":""}}}`,
			`{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","itemId":"message","delta":"old reply"}}`,
			`{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"id":"message","type":"agentMessage","text":"old reply"}}}`,
			`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`,
		}
		data := strings.Join(lines, "\n") + "\n"
		event := f.append(t, "shim.output", map[string]string{"stream": "stdout", "encoding": "base64", "data": base64.StdEncoding.EncodeToString([]byte(data))})
		start := 0
		for _, line := range lines {
			end := start + len(line) + 1
			f.state.Inbox = append(f.state.Inbox, shimcodex.Event{Identity: fmt.Sprintf("%s:stdout:%d:%d", journal, start, end), Cursor: event.Cursor, Raw: []byte(line)})
			start = end
		}
		f.state.Cursor = event.Cursor
		f.state.StreamOffset = uint64(len(data))
		f.state.PartialStart = f.state.StreamOffset
	}
	if err = db.UpsertSessionShim(ctx, SessionShimRow{SessionID: "s", ShimKey: "p", Runtime: "codex", RuntimeGeneration: 1, BootGeneration: "boot", HostBackend: "detached", DescriptorPath: f.receipt.DescriptorPath, SocketPath: f.receipt.SocketPath, JournalID: journal, HostPID: 100, ShimPID: 100, ProviderPID: 101}); err != nil {
		t.Fatal(err)
	}
	protocol, err := db.CodexProtocolStore(ctx, "s", "p", shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err = protocol.Commit(ctx, 0, f.state); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.receipt.Retired = true
	f.saveReceipt(t)
	if _, err = db.db.Exec(`UPDATE sessions SET state='completed' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *accountingFixture) append(t *testing.T, kind string, payload any) mesh.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event, err := f.journal.Append(mesh.Event{SchemaVersion: "1", ID: fmt.Sprintf("fixture-%d", time.Now().UnixNano()), Kind: kind, Time: time.Now().UTC(), SessionID: "s", Source: mesh.EventSource{Channel: "shim", Confidence: 1}, Actor: mesh.Actor{URN: "msg://service/local/shim", Kind: mesh.ActorService}, Subject: "urn:session:s", Generation: 1, ContentType: "application/json", Visibility: "private", Payload: raw}, false)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
func (f *accountingFixture) metadata(event mesh.Event) {
	raw, _ := json.Marshal(struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}{event.Kind, event.Payload})
	f.state.Inbox = append(f.state.Inbox, shimcodex.Event{Identity: f.state.Binding.Journal + ":" + event.Cursor + ":" + event.Kind, Cursor: event.Cursor, Raw: raw})
	f.state.Cursor = event.Cursor
}
func (f *accountingFixture) saveReceipt(t *testing.T) {
	t.Helper()
	raw, _ := json.Marshal(f.receipt)
	if err := os.WriteFile(filepath.Join(f.root, "placement.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *accountingFixture) settle(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	p, err := shimcodex.BuildDeliveryProjection(f.state)
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.Turns {
		turn := &p.Turns[i]
		if turn.OutputAcceptanceID != "" {
			continue
		}
		id := TurnOutputID("s", turn.StableOutputTurnID, "final", turn.CompletionSourceID, shimcodex.ProjectedTurnText(*turn))
		turn.OutputAcceptanceID = id
		output := events.TurnOutputEvent{OutputID: id, SessionID: "s", TurnID: turn.StableOutputTurnID, ProviderResultID: turn.CompletionSourceID, Kind: turnoutput.KindFinal, Runtime: "codex", StopReason: turn.StopReason, Text: shimcodex.ProjectedTurnText(*turn)}
		if f.routed {
			body, _ := json.Marshal(map[string]string{"text": output.Text})
			staged, e := f.store.StageTurnOutput(ctx, messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s"}, Payload: body, ContentType: "application/json", Metadata: map[string]string{"output_id": id, "session_id": "s", "turn_id": turn.StableOutputTurnID, "kind": "final", "provider_result_id": turn.CompletionSourceID}})
			if e != nil {
				t.Fatal(e)
			}
			turn.OutboxMessageIDs = []string{staged.ID}
			output.MessageID = staged.ID
			output.Text = ""
		}
		payload, _ := json.Marshal(output)
		if _, _, err = f.store.InsertEvent(events.ScopeSession, "s", events.KindSessionTurnOutput, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	next := f.state
	next.Revision++
	p.ProtocolRevision = next.Revision
	p.DeliveredHighWater = f.state.Cursor
	next.Inbox = nil
	next.Delivery = &p
	protocol, err := f.store.CodexProtocolStore(ctx, "s", "p", shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err = protocol.CommitDelivery(ctx, f.state, next); err != nil {
		t.Fatal(err)
	}
	f.state = next
	if f.routed {
		for _, turn := range p.Turns {
			for _, id := range turn.OutboxMessageIDs {
				if _, err = f.store.AttachChannelMessage(ctx, channels.ExistingMessage{MessageID: id, SessionID: "s", Channel: "ops", Actor: messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "turn-router"}}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func accountingCode(t *testing.T, err error, want string) {
	t.Helper()
	var failure *CodexAccountingFailure
	if !errors.As(err, &failure) || failure.Code != want {
		t.Fatalf("refusal %v, want %s", err, want)
	}
}

func TestCodexReplacementAccountingHistoricalReceiptAndSameTransaction(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprintf("reply=%v", reply), func(t *testing.T) {
			f := newAccountingFixture(t, reply)
			ctx := context.Background()
			_ = f.journal.Close()
			if _, err := f.store.LoadVerifiedCodexDelivery(ctx, f.state, f.state.Cursor); err == nil {
				t.Fatal("active issuer accepted terminal historical session")
			}
			proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt)
			if err != nil {
				t.Fatal(err)
			}
			if proof.Reference().Revision != f.state.Revision || proof.Reference().JournalHighWater != f.state.Cursor {
				t.Fatal("evidence reference mismatch")
			}
			conn, err := f.store.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()
			if err = f.store.ValidateCodexReplacementAccountingTx(ctx, conn, proof, f.receipt); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err = conn.QueryRowContext(ctx, `SELECT state_json FROM codex_shim_protocol WHERE session_id='s'`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(f.state)
			if raw != string(before) {
				t.Fatal("historical accounting mutated private ledger")
			}
			if _, err = conn.ExecContext(ctx, `UPDATE codex_shim_protocol SET revision='999' WHERE session_id='s'`); err != nil {
				t.Fatal(err)
			}
			accountingCode(t, f.store.ValidateCodexReplacementAccountingTx(ctx, conn, proof, f.receipt), "snapshot_changed")
		})
	}
}

func TestCodexReplacementAccountingRejectsTamperedEvidenceAndForeignProof(t *testing.T) {
	f := newAccountingFixture(t, true)
	_ = f.journal.Close()
	ctx := context.Background()
	proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	accountingCode(t, other.ValidateCodexReplacementAccountingTx(ctx, other.db, proof, f.receipt), "proof_mismatch")
	accountingCode(t, f.store.ValidateCodexReplacementAccountingTx(ctx, other.db, proof, f.receipt), "proof_store_mismatch")
	accountingCode(t, f.store.ValidateCodexReplacementAccountingTx(ctx, f.store.db, &CodexReplacementAccounting{}, f.receipt), "proof_mismatch")
	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE events SET payload_json=json_set(payload_json,'$.text','tampered') WHERE kind=?`, events.KindSessionTurnOutput); err != nil {
		t.Fatal(err)
	}
	accountingCode(t, f.store.ValidateCodexReplacementAccountingTx(ctx, tx, proof, f.receipt), "proof_changed")
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	f.receipt.Epoch = "changed"
	f.saveReceipt(t)
	accountingCode(t, f.store.ValidateCodexReplacementAccountingTx(ctx, f.store.db, proof, proof.receipt), "custody_changed")
}

func TestCodexReplacementAccountingRetainsUnknownHistoryAndPendingRetry(t *testing.T) {
	for _, kind := range []string{"active", "partial", "inbox", "effect", "retry", "torn", "unknown", "tail-output"} {
		t.Run(kind, func(t *testing.T) {
			f := newAccountingFixture(t, true)
			ctx := context.Background()
			observed := f.state
			switch kind {
			case "active":
				observed.ActiveTurn = "uncertain"
			case "partial":
				observed.Partial = []byte("x")
				observed.StreamOffset++
				observed.PartialStart = observed.StreamOffset - 1
			case "inbox":
				observed.Inbox = []shimcodex.Event{{Identity: "retained", Cursor: f.state.Cursor, Raw: json.RawMessage(`{}`)}}
			case "effect":
				observed.Operations[0].EffectUnknown = true
			case "retry":
				if err := f.store.WriteTurnOutputRetry(strings.Repeat("a", 64), []byte(`{"version":1,"session_id":"s"}`)); err != nil {
					t.Fatal(err)
				}
			case "torn":
				_ = f.journal.Close()
				file, err := os.OpenFile(filepath.Join(f.root, "j", "00000001.seg"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.Write([]byte{0, 0})
				_ = file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "unknown":
				f.append(t, "shim.unknown", map[string]string{"private": "retained"})
			case "tail-output":
				f.append(t, "shim.output", map[string]string{"stream": "stdout", "encoding": "base64", "data": base64.StdEncoding.EncodeToString([]byte("incomplete"))})
			}
			_ = f.journal.Close()
			before, err := os.ReadFile(filepath.Join(f.root, "j", "00000001.seg"))
			if err != nil {
				t.Fatal(err)
			}
			if proof, err := f.store.AccountCodexReplacement(ctx, observed, f.receipt); err == nil || proof != nil {
				t.Fatal("uncertain history earned accounting proof")
			}
			after, err := os.ReadFile(filepath.Join(f.root, "j", "00000001.seg"))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("historical reader recovered or drained journal")
			}
		})
	}
}

func TestCodexReplacementAccountingScansCompactedHistoryAndDetectsLostAcceptance(t *testing.T) {
	f := newAccountingFixture(t, true)
	ctx := context.Background()
	// Reopen the public session only inside this synthetic fixture to exercise
	// the normal active delivery CAS before archiving it again.
	if _, err := f.store.db.Exec(`UPDATE sessions SET state='running' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	previous := f.state
	for i := 0; i < 600; i++ {
		f.metadata(f.append(t, "shim.attached", map[string]int{"epoch": i + 2}))
	}
	f.state.Revision++
	raw, _ := json.Marshal(f.state)
	if _, err := f.store.db.Exec(`UPDATE codex_shim_protocol SET state_json=?,revision=? WHERE session_id='s'`, string(raw), strconv.FormatUint(f.state.Revision, 10)); err != nil {
		t.Fatal(err)
	}
	if previous.Delivery == nil {
		t.Fatal("missing initial actual receipt")
	}
	f.settle(t)
	_ = f.journal.Close()
	if f.state.Delivery.BaseReceiptSHA256 == "" || len(f.state.Delivery.Turns) != 0 {
		t.Fatal("fixture did not compact old native turn")
	}
	if _, err := f.store.db.Exec(`UPDATE sessions SET state='completed' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`DELETE FROM events WHERE kind=?`, events.KindSessionTurnOutput); err != nil {
		t.Fatal(err)
	}
	if proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt); err == nil || proof != nil {
		t.Fatal("compacted receipt digest substituted for missing historical acceptance")
	}
}

func TestCodexReplacementAccountingRequiresRealRoutedOutboxSettlement(t *testing.T) {
	f := newAccountingFixture(t, true, true)
	_ = f.journal.Close()
	ctx := context.Background()
	proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt)
	if err != nil {
		t.Fatal(err)
	}
	messageID := f.state.Delivery.Turns[0].OutboxMessageIDs[0]
	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET payload='{"text":"foreign body"}' WHERE id=?`, messageID); err != nil {
		t.Fatal(err)
	}
	accountingCode(t, f.store.ValidateCodexReplacementAccountingTx(ctx, tx, proof, f.receipt), "proof_changed")
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.db.Exec(`DELETE FROM channel_publications WHERE message_id=?`, messageID); err != nil {
		t.Fatal(err)
	}
	if candidate, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt); err == nil || candidate != nil {
		t.Fatal("missing channel attachment accounted as settled outbox")
	}
	if _, err = f.store.db.Exec(`UPDATE messages SET routing_staged=1 WHERE id=?`, messageID); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.AccountCodexReplacement(ctx, f.state, f.receipt)
	accountingCode(t, err, "public_pending")
}

func TestCodexReplacementAccountingRejectsSymlinkJournalAndFutureHighwater(t *testing.T) {
	f := newAccountingFixture(t, false)
	_ = f.journal.Close()
	ctx := context.Background()
	observed := f.state
	observed.ReplayHighWater = observed.Binding.Journal + ":999"
	raw, _ := json.Marshal(observed)
	if _, err := f.store.db.Exec(`UPDATE codex_shim_protocol SET state_json=? WHERE session_id='s'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.AccountCodexReplacement(ctx, observed, f.receipt)
	accountingCode(t, err, "journal_gap")
	raw, _ = json.Marshal(f.state)
	if _, err = f.store.db.Exec(`UPDATE codex_shim_protocol SET state_json=? WHERE session_id='s'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	segment := filepath.Join(f.root, "j", "00000001.seg")
	target := filepath.Join(f.root, "preserved-segment")
	if err = os.Rename(segment, target); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, segment); err != nil {
		t.Fatal(err)
	}
	if proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt); err == nil || proof != nil {
		t.Fatal("symlink journal minted accounting")
	}
}

func TestCodexReplacementAccountingAuthenticSettledExitAndUnaccountedEffects(t *testing.T) {
	f := newAccountingFixture(t, true)
	ctx := context.Background()
	if _, err := f.store.db.Exec(`UPDATE sessions SET state='running' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	exit := shim.Exit{Status: 0, Cause: "provider_exit"}
	event := f.append(t, "shim.exit", exit)
	previous := f.state.Revision
	f.state.Revision++
	f.state.Exit = &exit
	f.state.ExitCursor = event.Cursor
	f.state.Cursor = event.Cursor
	f.state.Inbox = []shimcodex.Event{{Identity: f.state.Binding.Journal + ":exit:" + event.Cursor, Cursor: event.Cursor, Raw: event.Payload}}
	protocol, err := f.store.CodexProtocolStore(ctx, "s", "p", shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err = protocol.Commit(ctx, previous, f.state); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	if _, err = f.store.db.Exec(`UPDATE sessions SET state='completed' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.AccountCodexReplacement(ctx, f.state, f.receipt); err != nil {
		t.Fatal(err)
	}
	// A trailing input intent without a matching outcome cannot be dismissed
	// because the provider has an authentic exit or the private inbox is empty.
	f.append(t, "shim.inject_intent", map[string]string{"key": "codex-" + strconv.FormatUint(shimcodex.FirstID, 10), "fingerprint": "another", "mode": "input", "delivery": "immediate"})
	_ = f.journal.Close()
	if proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt); err == nil || proof != nil {
		t.Fatal("extra historical effect earned replacement accounting")
	}
}
