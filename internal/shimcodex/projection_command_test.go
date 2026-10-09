//go:build !windows

package shimcodex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func commandNotification(method, status, output string) string {
	item := map[string]any{"id": "cmd", "type": "commandExecution", "command": "printf synthetic", "cwd": "/fixture", "commandActions": []any{}, "source": "agent", "status": status, "aggregatedOutput": output, "durationMs": nil, "exitCode": nil, "processId": "fixture-process", "pluginId": nil, "scriptPath": nil}
	if status != "inProgress" {
		item["exitCode"], item["durationMs"] = 0, 2
	}
	body, _ := json.Marshal(map[string]any{"method": method, "params": map[string]any{"threadId": "th", "turnId": "tu", "item": item}})
	return string(body)
}

func commandProjectionStart(t *testing.T) Projection {
	t.Helper()
	p, err := ProjectFrozenSource(projectionFixture(), projectedSource(1, `{"method":"turn/started","params":{"threadId":"th","turn":{"id":"tu","status":"inProgress"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	p, err = ProjectFrozenSource(p, projectedSource(2, commandNotification("item/started", "inProgress", "")))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommandProjectionRetainedOpenTurnRoundTripAndCompletion(t *testing.T) {
	p := commandProjectionStart(t)
	if p.Turns[0].Phase != "open" || p.Turns[0].Items[0].Command == nil || ProjectedTurnText(p.Turns[0]) != "" || p.DeliveredHighWater != "" {
		t.Fatal("tool start fabricated public reply or lost private obligation")
	}
	startBytes := append([]byte(nil), p.Turns[0].Items[0].Command.StartedParams...)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = DecodeProjection(raw)
	if err != nil || !bytes.Equal(p.Turns[0].Items[0].Command.StartedParams, startBytes) {
		t.Fatal("private command bytes changed on reopen", err)
	}
	bodies := []string{
		`{"method":"item/commandExecution/outputDelta","params":{"threadId":"th","turnId":"tu","itemId":"cmd","delta":"private tool 世界\n"}}`,
		commandNotification("item/completed", "completed", "independent native aggregate"),
		`{"method":"item/started","params":{"threadId":"th","turnId":"tu","item":{"id":"answer","type":"agentMessage","text":"","phase":"final_answer"}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"th","turnId":"tu","itemId":"answer","delta":"public final"}}`,
		`{"method":"item/completed","params":{"threadId":"th","turnId":"tu","item":{"id":"answer","type":"agentMessage","text":"public final","phase":"final_answer"}}}`,
		`{"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","status":"completed"}}}`,
	}
	for i, body := range bodies {
		before, _ := json.Marshal(p)
		next, err := ProjectFrozenSource(p, projectedSource(i+3, body))
		if err != nil {
			t.Fatal(i, err)
		}
		after, _ := json.Marshal(p)
		if !bytes.Equal(before, after) {
			t.Fatal("projection mutated private input")
		}
		p = next
	}
	tool := p.Turns[0].Items[0]
	if tool.TextBytes != "private tool 世界\n" || tool.CompletedSourceID == "" || !bytes.Contains(tool.Command.CompletedParams, []byte("independent native aggregate")) || !bytes.Equal(tool.Command.StartedParams, startBytes) {
		t.Fatal("streamed or aggregate native tool bytes lost")
	}
	if ProjectedTurnText(p.Turns[0]) != "public final" || p.Turns[0].OutputAcceptanceID != "" || p.Terminal != nil {
		t.Fatal("command trace contaminated reply or pure projection minted receipt")
	}
	raw, _ = json.Marshal(p)
	if _, err = DecodeProjection(raw); err != nil {
		t.Fatal(err)
	}
}

func TestCommandProjectionRefusesForeignAndUnsupportedLifecycle(t *testing.T) {
	for _, kind := range []string{"thread", "turn", "item", "changed_command", "changed_process", "wrong_delta_kind", "early_turn_end", "late_delta", "duplicate_completion", "unknown_field", "unknown_status", "unknown_union", "missing_started_bytes"} {
		t.Run(kind, func(t *testing.T) {
			p := commandProjectionStart(t)
			body := commandNotification("item/completed", "completed", "done")
			code := "item_mismatch"
			switch kind {
			case "thread":
				body = strings.Replace(body, `"threadId":"th"`, `"threadId":"other"`, 1)
				code = "turn_mismatch"
			case "turn":
				body = strings.Replace(body, `"turnId":"tu"`, `"turnId":"other"`, 1)
				code = "turn_mismatch"
			case "item":
				body = strings.Replace(body, `"id":"cmd"`, `"id":"other"`, 1)
			case "changed_command":
				body = strings.Replace(body, "printf synthetic", "other command", 1)
			case "changed_process":
				body = strings.Replace(body, "fixture-process", "other-process", 1)
			case "wrong_delta_kind":
				body = `{"method":"item/agentMessage/delta","params":{"threadId":"th","turnId":"tu","itemId":"cmd","delta":"wrong"}}`
			case "early_turn_end":
				body = `{"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","status":"completed"}}}`
				code = "item_pending"
			case "late_delta", "duplicate_completion":
				var err error
				p, err = ProjectFrozenSource(p, projectedSource(3, body))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "late_delta" {
					body = `{"method":"item/commandExecution/outputDelta","params":{"threadId":"th","turnId":"tu","itemId":"cmd","delta":"late"}}`
				}
			case "unknown_field":
				body = strings.Replace(body, `"command":"`, `"future":true,"command":"`, 1)
				code = "output_unsupported"
			case "unknown_status":
				body = strings.Replace(body, `"status":"completed"`, `"status":"future"`, 1)
				code = "output_unsupported"
			case "unknown_union":
				body = strings.Replace(body, `"type":"commandExecution"`, `"type":"futureTool"`, 1)
				code = "output_unsupported"
			case "missing_started_bytes":
				p.Turns[0].Items[0].Command.StartedParams = nil
				if ValidateProjection(p) == nil {
					t.Fatal("forged private command accepted")
				}
				return
			}
			position := len(p.Sources) + 1
			before, _ := json.Marshal(p)
			next, err := ProjectFrozenSource(p, projectedSource(position, body))
			if code == "output_unsupported" {
				if err != nil || next.Sources[len(next.Sources)-1].Disposition != "retained_unsupported" {
					t.Fatal("future shape dropped", err)
				}
				state := State{Version: Version, Binding: p.Binding, Revision: p.ProtocolRevision, Delivery: &p, Cursor: fmt.Sprintf("j:%d", position), Inbox: []Event{projectedSource(position, body)}}
				if _, err = BuildDeliveryProjection(state); !HasCode(err, code) {
					t.Fatal("unsupported command drained", err)
				}
			} else if !HasCode(err, code) {
				t.Fatal("unsafe lifecycle accepted", kind, err)
			}
			after, _ := json.Marshal(p)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed source")
			}
		})
	}
}
