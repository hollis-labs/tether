//go:build !windows

package shimcodex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func projectionFixture() Projection {
	return Projection{Version: ProjectionVersion, Binding: Binding{Session: "s", Instance: "i", Generation: 1, Operation: "p", Attempt: "a", Fingerprint: "f", Journal: "j"}, ProtocolRevision: 1, JournalIdentity: "j"}
}
func projectedSource(position int, body string) Event {
	return Event{Identity: fmt.Sprintf("fixture:%d", position), Cursor: fmt.Sprintf("j:%d", position), Raw: []byte(body)}
}
func TestProjectionPureTurnReplayAndRetainedUnsupported(t *testing.T) {
	p := projectionFixture()
	bodies := []string{
		`{"method":"turn/started","params":{"threadId":"th","turn":{"id":"tu","status":"inProgress"}}}`,
		`{"method":"item/started","params":{"threadId":"th","turnId":"tu","item":{"id":"it","type":"agentMessage","text":""}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"th","turnId":"tu","itemId":"it","delta":"hello"}}`,
		`{"method":"item/completed","params":{"threadId":"th","turnId":"tu","item":{"id":"it","type":"agentMessage","text":"hello"}}}`,
		`{"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","status":"completed"}}}`,
	}
	for i, body := range bodies {
		before, _ := json.Marshal(p)
		candidate, err := ProjectFrozenSource(p, projectedSource(i+1, body))
		if err != nil {
			t.Fatal(i, err)
		}
		after, _ := json.Marshal(p)
		if string(before) != string(after) {
			t.Fatal("mutated input")
		}
		p = candidate
	}
	if p.Turns[0].Items[0].TextBytes != "hello" || p.Turns[0].Phase != "completed" || p.DeliveredHighWater != "" || p.Terminal != nil {
		t.Fatal("candidate fabricated delivery/terminal or lost text")
	}
	same, err := ProjectFrozenSource(p, projectedSource(3, bodies[2]))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(same)
	if string(a) != string(b) {
		t.Fatal("duplicate projected twice")
	}
	splice := projectedSource(3, strings.Replace(bodies[2], "hello", "other", 1))
	if _, err := ProjectFrozenSource(p, splice); !HasCode(err, "source_conflict") {
		t.Fatal("source splice accepted", err)
	}
	unknown := projectedSource(6, `{"method":"item/started","params":{"threadId":"th","turnId":"tu","item":{"id":"tool","type":"commandExecution"}}}`)
	p, err = ProjectFrozenSource(p, unknown)
	if err != nil || p.Sources[5].Disposition != "retained_unsupported" {
		t.Fatal("unsupported union dropped", err)
	}
}
func TestProjectionClosedSchemaBoundsAndContinuity(t *testing.T) {
	for _, kind := range []string{"duplicate", "unknown", "counter", "overflow", "frame", "gap", "thread", "metadata_splice", "exit", "nesting", "collection"} {
		t.Run(kind, func(t *testing.T) {
			p := projectionFixture()
			raw, _ := json.Marshal(p)
			switch kind {
			case "duplicate":
				raw = append([]byte(`{"version":"codex-output-v1",`), raw[1:]...)
			case "unknown":
				raw = append([]byte(`{"extra":true,`), raw[1:]...)
			case "counter":
				raw = []byte(strings.Replace(string(raw), `"protocol_revision":"1"`, `"protocol_revision":"01"`, 1))
			case "overflow":
				raw = []byte(strings.Replace(string(raw), `"protocol_revision":"1"`, `"protocol_revision":"18446744073709551616"`, 1))
			case "frame":
				_, err := ProjectFrozenSource(p, projectedSource(1, strings.Repeat(" ", ProjectionFrameBytes+1)))
				if err == nil {
					t.Fatal("unbounded frame")
				}
				return
			case "gap":
				_, err := ProjectFrozenSource(p, projectedSource(2, `{"method":"unknown"}`))
				if !HasCode(err, "source_gap") {
					t.Fatal("cursor gap hidden", err)
				}
				return
			case "thread":
				p.ActiveThreadID = "original"
				_, err := ProjectFrozenSource(p, projectedSource(1, `{"method":"turn/started","params":{"threadId":"other","turn":{"id":"t","status":"inProgress"}}}`))
				if !HasCode(err, "turn_mismatch") {
					t.Fatal("foreign thread", err)
				}
				return
			case "metadata_splice":
				_, err := ProjectFrozenSource(p, projectedSource(1, `{"kind":"shim.attached","payload":{}}`))
				if !HasCode(err, "source_conflict") {
					t.Fatal("metadata identity borrowed", err)
				}
				return
			case "nesting":
				body := strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)
				if _, err := ProjectFrozenSource(p, projectedSource(1, body)); err == nil {
					t.Fatal("deep source allocated")
				}
				return
			case "collection":
				raw = []byte(strings.Replace(string(raw), `"outstanding_input_ids":null`, `"outstanding_input_ids":[`+strings.Repeat(`"x",`, ProjectionSources)+`"x"]`, 1))
			case "exit":
				p, err := ProjectFrozenSource(p, Event{Identity: "j:exit:j:1", Cursor: "j:1", Raw: []byte(`{"status":7}`)})
				if err != nil || p.Terminal != nil || p.Sources[0].Disposition != "retained_unsupported" {
					t.Fatal("pure candidate authenticated exit", err)
				}
				return
			}
			if _, err := DecodeProjection(raw); err == nil {
				t.Fatal("invalid projection accepted")
			}
		})
	}
}
func TestProjectionMultipleLinesOneCursorKeepByteContinuity(t *testing.T) {
	p := projectionFixture()
	offset := uint64(0)
	for _, body := range []string{`{"method":"unknown/a"}`, `{"method":"unknown/b"}`} {
		end := offset + uint64(len(body)) + 1
		e := Event{Identity: fmt.Sprintf("j:stdout:%d:%d", offset, end), Cursor: "j:1", Raw: []byte(body)}
		next, err := ProjectFrozenSource(p, e)
		if err != nil {
			t.Fatal(err)
		}
		p = next
		offset = end
	}
	if len(p.Sources) != 2 || p.StdoutOffset != offset {
		t.Fatal("same-cursor output erased")
	}
	_, err := ProjectFrozenSource(p, Event{Identity: fmt.Sprintf("j:stdout:%d:%d", offset+1, offset+24), Cursor: "j:1", Raw: []byte(`{"method":"unknown/c"}`)})
	if !HasCode(err, "source_gap") {
		t.Fatal("stdout gap hidden", err)
	}
}

func TestProjectionNeverReplacesInvalidUnicodeText(t *testing.T) {
	for _, text := range []string{`\ud800`, `\udc00`, string([]byte{0xff})} {
		raw := []byte(`{"method":"unknown","params":{"text":"` + text + `"}}`)
		if _, err := ProjectFrozenSource(projectionFixture(), projectedSource(1, string(raw))); err == nil {
			t.Fatal("invalid text replaced")
		}
	}
	raw := `{"method":"unknown","params":{"text":"\ud83d\ude00"}}`
	if _, err := ProjectFrozenSource(projectionFixture(), projectedSource(1, raw)); err != nil {
		t.Fatal("valid surrogate pair refused", err)
	}
}

func TestProjectionUnsupportedShapesRetainSourceWithoutEffects(t *testing.T) {
	bodies := []string{
		`{"method":"turn/started","params":{"threadId":"th","turn":{"id":"tu","status":"inProgress","extra":true}}}`,
		`{"method":"item/started","params":{"threadId":"th","turnId":"tu","item":{"id":"i","type":"commandExecution"}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"th","turnId":"tu","itemId":"i","delta":"text","extra":true}}`,
		`{"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","status":"completed","extra":true}}}`,
	}
	for _, body := range bodies {
		before := projectionFixture()
		next, err := ProjectFrozenSource(before, projectedSource(1, body))
		if err != nil || len(next.Sources) != 1 || next.Sources[0].Disposition != "retained_unsupported" || next.AcceptedSourceCursor != "j:1" {
			t.Fatalf("unsupported source not retained: %+v %v", next, err)
		}
		if next.ActiveTurnID != "" || len(next.Turns) != 0 || next.Terminal != nil || next.DeliveredHighWater != "" || len(before.Sources) != 0 {
			t.Fatal("unsupported source manufactured effects or mutated input")
		}
	}
}
