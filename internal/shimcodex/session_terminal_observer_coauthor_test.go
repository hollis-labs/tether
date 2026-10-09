//go:build !windows

package shimcodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/hollis-labs/substrate/harness/shim"
)

func TestSessionTerminalObserverRefusalPreservesNativeEvidence(t *testing.T) {
	for _, answered := range []bool{true, false} {
		name := "missing_answer"
		if answered {
			name = "answered"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			engine, stored := newEngine(t)
			boundThread(t, engine)
			before := engine.Snapshot()
			transport := &fixtureTransport{}
			session, err := NewSession(engine, transport, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Stop(ctx) })
			const turnID = "terminal-observer-turn"
			var accepted State
			transport.inject = func(callCtx context.Context, key string, data []byte) error {
				var request struct {
					ID     uint64 `json:"id"`
					Method string `json:"method"`
				}
				if json.Unmarshal(data, &request) != nil || request.ID != before.NextID || request.Method != "turn/start" || key != fmt.Sprintf("codex-%d", request.ID) {
					t.Fatal("fixture did not receive the exact reserved turn request")
				}
				native := ""
				if answered {
					native = fmt.Sprintf("{\"id\":%d,\"result\":{\"turn\":{\"id\":%q}}}\n", request.ID, turnID)
				}
				native += fmt.Sprintf("{\"method\":\"turn/started\",\"params\":{\"threadId\":%q,\"turn\":{\"id\":%q,\"status\":\"inProgress\"}}}\n", before.ThreadID, turnID)
				native += fmt.Sprintf("{\"method\":\"turn/completed\",\"params\":{\"threadId\":%q,\"turn\":{\"id\":%q,\"status\":\"completed\"}}}\n{", before.ThreadID, turnID)
				if err := session.AcceptOutput(callCtx, "j:3", "stdout", []byte(native)); err != nil {
					t.Fatal(err)
				}
				// This is the authenticated reader path. Local Finish alone must
				// never create an exit or turn incomplete carry into public output.
				if err := engine.AcceptExit(callCtx, "j:4", shim.Exit{Status: 7}); err != nil {
					t.Fatal(err)
				}
				accepted = engine.Snapshot()
				session.Finish(7, fail("protocol_truncated"))
				return errors.New("synthetic local receipt refusal")
			}
			result, err := session.Call(ctx, "turn/start", map[string]any{"threadId": before.ThreadID, "input": []any{}})
			if result != nil || !HasCode(err, "outcome_unknown") {
				t.Fatal("terminal observer refusal became RPC success")
			}
			code, err := session.Wait()
			if code != 7 || !HasCode(err, "protocol_truncated") {
				t.Fatal("terminal observer lost the authenticated exit or truncation error")
			}
			durable, err := stored.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(durable, accepted) || !reflect.DeepEqual(engine.Snapshot(), durable) {
				t.Fatal("local receipt refusal changed durably accepted native evidence")
			}
			if durable.ThreadID != before.ThreadID || durable.NextID != before.NextID+1 || durable.LastTerminal != turnID || durable.ActiveTurn != "" {
				t.Fatal("terminal observer changed thread, request sequence, or turn identity")
			}
			if durable.Exit == nil || durable.Exit.Status != 7 || durable.ExitCursor != "j:4" || durable.Cursor != "j:4" || string(durable.Partial) != "{" || durable.PartialStart+1 != durable.StreamOffset {
				t.Fatal("terminal observer erased the authenticated exit or truncated carry")
			}
			op := find(&durable, before.NextID)
			if op == nil || op.RPCError != nil {
				t.Fatal("terminal observer lost its exact native operation")
			}
			if answered {
				var response struct {
					Turn struct {
						ID string `json:"id"`
					} `json:"turn"`
				}
				if op.Phase != Answered || op.EffectUnknown || json.Unmarshal(op.Result, &response) != nil || response.Turn.ID != turnID {
					t.Fatal("local receipt refusal erased or fabricated the matching native answer")
				}
			} else if op.Phase != Attempted || len(op.Result) != 0 {
				t.Fatal("completion and provider exit fabricated a missing native answer")
			}
			if len(durable.Inbox) <= len(before.Inbox) || durable.Inbox[len(durable.Inbox)-1].Identity != "j:exit:j:4" || durable.Delivery != nil {
				t.Fatal("terminal observer drained private evidence or fabricated public delivery")
			}
			if _, err := session.Call(ctx, "turn/start", map[string]any{"threadId": before.ThreadID}); !HasCode(err, "detached") {
				t.Fatal("finished controller admitted another provider request")
			}
			transport.mu.Lock()
			injects := transport.injects
			transport.mu.Unlock()
			if injects != 1 || !reflect.DeepEqual(engine.Snapshot(), durable) {
				t.Fatal("terminal observer resent input or changed retained evidence")
			}
		})
	}
}
