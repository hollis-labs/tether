//go:build !windows

package shimcodex

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const emptyReasoningStart = `{"method":"item/started","params":{"threadId":"th","turnId":"tu","startedAtMs":1,"item":{"id":"reason","type":"reasoning","content":[],"summary":[]}}}`
const emptyReasoningEnd = `{"method":"item/completed","params":{"threadId":"th","turnId":"tu","completedAtMs":2,"item":{"id":"reason","type":"reasoning","content":[],"summary":[]}}}`
const terminalObservation = `{"method":"item/commandExecution/terminalInteraction","params":{"threadId":"th","turnId":"tu","itemId":"cmd","processId":"fixture-process","stdin":"private synthetic input > & <\n"}}`

func observationStep(t *testing.T, p Projection, body string) Projection {
	t.Helper()
	before, _ := json.Marshal(p)
	next, err := ProjectFrozenSource(p, projectedSource(len(p.Sources)+1, body))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(p)
	if !bytes.Equal(before, after) {
		t.Fatal("private source projection mutated")
	}
	return next
}

func TestPrivateCodexObservationsReopenAndFinalReply(t *testing.T) {
	p := commandProjectionStart(t)
	p = observationStep(t, p, emptyReasoningStart)
	raw, _ := json.Marshal(p)
	p, err := DecodeProjection(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Turns[0].Items[1].CompletedSourceID != "" || ProjectedTurnText(p.Turns[0]) != "" {
		t.Fatal("unfinished reasoning lost or published")
	}
	for _, body := range []string{emptyReasoningEnd, terminalObservation, commandNotification("item/completed", "completed", "private aggregate"),
		`{"method":"item/started","params":{"threadId":"th","turnId":"tu","item":{"id":"answer","type":"agentMessage","text":"","phase":"final_answer"}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"th","turnId":"tu","itemId":"answer","delta":"public answer"}}`,
		`{"method":"item/completed","params":{"threadId":"th","turnId":"tu","item":{"id":"answer","type":"agentMessage","text":"public answer","phase":"final_answer"}}}`,
		`{"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","status":"completed"}}}`} {
		p = observationStep(t, p, body)
	}
	raw, _ = json.Marshal(p)
	p, err = DecodeProjection(raw)
	if err != nil {
		t.Fatal(err)
	}
	interaction := p.Turns[0].Items[0].Command.Interactions[0]
	var message Message
	if json.Unmarshal([]byte(terminalObservation), &message) != nil || !bytes.Equal(interaction.Params, message.Params) {
		t.Fatal("observed native stdin bytes changed on checkpoint")
	}
	if ProjectedTurnText(p.Turns[0]) != "public answer" || p.Turns[0].Items[1].Phase != "completed" || p.ActiveTurnID != "" || p.DeliveredHighWater != "" || p.Turns[0].OutputAcceptanceID != "" {
		t.Fatal("private observation contaminated final reply or fabricated acceptance")
	}
	p.Turns[0].Items[0].Command.Interactions[0].Params = []byte(strings.Replace(string(interaction.Params), "fixture-process", "other-process", 1))
	if ValidateProjection(p) == nil {
		t.Fatal("checkpoint process splice accepted")
	}
}

func TestPrivateCodexObservationsRefuseUnsupportedAndForeignLifecycle(t *testing.T) {
	for _, kind := range []string{"reasoning_content", "reasoning_summary", "reasoning_unknown", "reasoning_missing_array", "reasoning_null_array", "reasoning_foreign_thread", "reasoning_foreign_turn", "reasoning_missing_start", "reasoning_duplicate_start", "reasoning_duplicate_end", "reasoning_unfinished", "interaction_unknown", "interaction_missing_stdin", "interaction_null_stdin", "interaction_foreign_thread", "interaction_foreign_turn", "interaction_foreign_item", "interaction_foreign_process", "interaction_missing_process_witness", "interaction_late"} {
		t.Run(kind, func(t *testing.T) {
			p := commandProjectionStart(t)
			body, code := emptyReasoningStart, "output_unsupported"
			switch kind {
			case "reasoning_content":
				body = strings.Replace(body, `"content":[]`, `"content":["private text"]`, 1)
			case "reasoning_summary":
				body = strings.Replace(body, `"summary":[]`, `"summary":["private summary"]`, 1)
			case "reasoning_unknown":
				body = strings.Replace(body, `"summary":[]`, `"summary":[],"future":true`, 1)
			case "reasoning_missing_array":
				body = strings.Replace(body, `,"summary":[]`, "", 1)
			case "reasoning_null_array":
				body = strings.Replace(body, `"summary":[]`, `"summary":null`, 1)
			case "reasoning_foreign_thread":
				body = strings.Replace(body, `"threadId":"th"`, `"threadId":"other"`, 1)
				code = "turn_mismatch"
			case "reasoning_foreign_turn":
				body = strings.Replace(body, `"turnId":"tu"`, `"turnId":"other"`, 1)
				code = "turn_mismatch"
			case "reasoning_missing_start":
				body, code = emptyReasoningEnd, "item_mismatch"
			case "reasoning_duplicate_start":
				p = observationStep(t, p, body)
				code = "item_mismatch"
			case "reasoning_duplicate_end":
				p = observationStep(t, p, body)
				p = observationStep(t, p, emptyReasoningEnd)
				body, code = emptyReasoningEnd, "item_mismatch"
			case "reasoning_unfinished":
				p = observationStep(t, p, body)
				p = observationStep(t, p, commandNotification("item/completed", "completed", ""))
				body, code = `{"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","status":"completed"}}}`, "item_pending"
			default:
				body = terminalObservation
				switch kind {
				case "interaction_unknown":
					body = strings.Replace(body, `"stdin":`, `"future":true,"stdin":`, 1)
				case "interaction_missing_stdin":
					body = `{"method":"item/commandExecution/terminalInteraction","params":{"threadId":"th","turnId":"tu","itemId":"cmd","processId":"fixture-process"}}`
				case "interaction_null_stdin":
					body = strings.Replace(body, `"private synthetic input > & <\n"`, `null`, 1)
				case "interaction_foreign_thread":
					body = strings.Replace(body, `"threadId":"th"`, `"threadId":"other"`, 1)
					code = "turn_mismatch"
				case "interaction_foreign_turn":
					body = strings.Replace(body, `"turnId":"tu"`, `"turnId":"other"`, 1)
					code = "turn_mismatch"
				case "interaction_foreign_item":
					body = strings.Replace(body, `"itemId":"cmd"`, `"itemId":"other"`, 1)
					code = "item_mismatch"
				case "interaction_foreign_process":
					body = strings.Replace(body, "fixture-process", "other-process", 1)
					code = "item_mismatch"
				case "interaction_missing_process_witness":
					p = projectionFixture()
					p = observationStep(t, p, `{"method":"turn/started","params":{"threadId":"th","turn":{"id":"tu","status":"inProgress"}}}`)
					p = observationStep(t, p, strings.Replace(commandNotification("item/started", "inProgress", ""), `"fixture-process"`, `null`, 1))
					code = "item_mismatch"
				case "interaction_late":
					p = observationStep(t, p, commandNotification("item/completed", "completed", ""))
					code = "item_mismatch"
				}
			}
			before, _ := json.Marshal(p)
			state := State{Version: Version, Binding: p.Binding, Revision: p.ProtocolRevision, Delivery: &p, Inbox: []Event{projectedSource(len(p.Sources)+1, body)}}
			stateBefore, _ := json.Marshal(state)
			if _, err := BuildDeliveryProjection(state); !HasCode(err, code) {
				t.Fatal("unsafe observation accepted", err)
			}
			after, _ := json.Marshal(p)
			stateAfter, _ := json.Marshal(state)
			if !bytes.Equal(before, after) || !bytes.Equal(stateBefore, stateAfter) {
				t.Fatal("refusal changed durable obligations")
			}
		})
	}
}
