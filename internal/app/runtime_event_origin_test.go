package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/harness/core/adapters/turnoutput"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

func nativeOriginExample() runtimeevents.Event {
	offset := int64(0)
	return runtimeevents.Event{
		SchemaVersion: "1", ID: "native-source", Time: time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
		App: "provider-app", SessionID: "native-session", TurnID: "native-turn", ParentID: "subagent-parent", RawOffset: &offset,
		Sequence: 71, Generation: 3, Process: runtimeevents.Process{PID: 7, Provider: "codex", Runtime: "jsonrpc-stdio", ProviderSessionID: "provider-session"},
		Source:  runtimeevents.Source{Channel: "native", Confidence: runtimeevents.ConfidenceExact},
		Scope:   &runtimeevents.Scope{WorkerID: "worker", TaskID: "task", Attempt: 2},
		Corr:    &runtimeevents.Corr{Traceparent: "traceparent", ThreadID: "thread", CausationID: "older-cause", Depth: 2},
		Actor:   &runtimeevents.Actor{Kind: "agent", URN: "msg://agent/example/author", Verified: "token", Role: "workflow", OnBehalfOf: []string{"msg://user/example/operator"}},
		Class:   &runtimeevents.Class{Sensitivity: "secret", PII: true, PayloadSchema: "native@1", Sanitized: true, Redactions: 7, Labels: []string{"label"}},
		Payload: json.RawMessage(`{"params":{"private_native_marker":"never-store"}}`),
	}
}

func TestDerivedRuntimeOriginPreservesOnlyEnvelope(t *testing.T) {
	native := nativeOriginExample()
	origin, err := derivedRuntimeOrigin(native)
	if err != nil {
		t.Fatal(err)
	}
	e := origin.apply(events.Event{SessionID: "host-session", Kind: "derived.kind", PayloadJSON: `{"count":1}`})
	v := e.Envelope
	if e.EventID != "" || e.SessionID != "host-session" || e.Kind != "derived.kind" || e.PayloadJSON != `{"count":1}` || e.At != native.Time {
		t.Fatalf("native event replaced approved derived event: %+v", e)
	}
	if v.ParentID != "subagent-parent" || v.Corr.CausationID != native.ID || v.Corr.Depth != 3 || v.Corr.Traceparent != "traceparent" || v.Corr.ThreadID != "thread" {
		t.Fatalf("causal/structural links conflated: %+v", v)
	}
	if v.Sequence != 71 || v.Generation != 3 || v.Process.ProviderSessionID != "provider-session" || v.Source.Channel != "native" || v.Dims.WorkerID != "worker" || v.Actor.Verified != "token" {
		t.Fatalf("producer metadata lost: %+v", v)
	}
	if v.Class.Sensitivity != "secret" || !v.Class.PII || v.Class.PayloadSchema != "" || v.Class.Sanitized || v.Class.Redactions != 0 || v.RetentionClass != "" || v.PayloadDigest != "" || !v.RecordedAt.IsZero() {
		t.Fatalf("native payload claims inherited or floors lost: %+v", v)
	}
	*origin.Envelope.RawOffset = 10
	origin.Envelope.Actor.OnBehalfOfChain[0] = "changed"
	origin.Envelope.Class.Labels[0] = "changed"
	if *native.RawOffset != 0 || native.Actor.OnBehalfOf[0] != "msg://user/example/operator" || native.Class.Labels[0] != "label" {
		t.Fatal("origin aliases native envelope")
	}
	raw, err := json.Marshal(origin)
	if err != nil || strings.Contains(string(raw), "private_native_marker") || strings.Contains(string(raw), "never-store") {
		t.Fatalf("raw native payload escaped: %s %v", raw, err)
	}
	native.Corr.Depth = math.MaxUint32
	if _, err := derivedRuntimeOrigin(native); err == nil {
		t.Fatal("causal depth overflow accepted")
	}
}

func TestDerivedRequestEnvelopeAndOutputFloors(t *testing.T) {
	svc, output := outputHarness(t, nil)
	native := nativeOriginExample()
	native.Class.Sensitivity = "private"
	native.Kind = runtimeevents.KindAgentPermissionRequested
	native.Payload = json.RawMessage(`{"request_id":1,"method":"item/tool/requestUserInput","params":{"private_native_marker":"never-store"}}`)
	output.observeRuntime(native)
	native.Kind = runtimeevents.KindAgentDelta
	native.ID = "native-delta"
	native.Sequence++
	native.Payload = json.RawMessage(`{"content":"approved answer","phase":"final"}`)
	output.observeRuntime(native)
	terminal := native
	terminal.Kind = runtimeevents.KindTurnCompleted
	terminal.ID = "native-terminal"
	terminal.Sequence++
	terminal.Class = &runtimeevents.Class{Sensitivity: "public"}
	terminal.Payload = nil
	output.observeRuntime(terminal)
	rows, err := svc.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var requests, outputs int
	for _, e := range rows {
		if strings.Contains(e.PayloadJSON, "private_native_marker") || strings.Contains(e.PayloadJSON, "never-store") || e.Kind == string(runtimeevents.KindAgentDelta) {
			t.Fatalf("raw native payload/kind persisted: %+v", e)
		}
		switch e.Kind {
		case store.EnvironmentRequestKind:
			requests++
			if e.EventID == native.ID || !strings.HasPrefix(e.EventID, "evt_") || e.Envelope.ParentID != "subagent-parent" || e.Envelope.Corr.CausationID == "" || e.Envelope.TurnID != "native-turn" || !e.At.Equal(native.Time) {
				t.Fatalf("request envelope lost: %+v", e)
			}
		case events.KindSessionTurnOutput:
			outputs++
			if e.Envelope.Class.Sensitivity != "private" || !e.Envelope.Class.PII || e.Envelope.Class.Sanitized || e.Envelope.Class.PayloadSchema != "" || e.Envelope.Corr.CausationID != "native-terminal" || e.Envelope.Sequence != terminal.Sequence {
				t.Fatalf("terminal marker weakened accumulated provenance: %+v", e)
			}
		}
	}
	if requests != 2 || outputs != 1 {
		t.Fatalf("request open/close and derived output missing: %d/%d", requests, outputs)
	}
	base, err := svc.Store.EnvironmentSnapshot(context.Background(), "s1")
	if err != nil || len(base.Requests) != 1 || base.Requests[0].Open {
		t.Fatalf("request bookkeeping changed: %+v %v", base, err)
	}
}

func TestDerivedOutputOriginSurvivesJournalReplay(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	native := nativeOriginExample()
	native.Class.Sensitivity = "private"
	origin, err := derivedRuntimeOrigin(native)
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	job := &turnOutputWrite{row: *row, origin: origin, result: turnoutput.Output{TurnID: "native-turn", Kind: turnoutput.KindFinal, Text: "approved derived text"}}
	if err := job.journal(svc); err != nil {
		t.Fatal(err)
	}
	data, err := svc.Store.ReadTurnOutputRetry(job.journalID)
	if err != nil || strings.Contains(string(data), "private_native_marker") {
		t.Fatalf("raw native payload in retry journal: %s %v", data, err)
	}
	replayed, err := readOutputRetry(svc, job.journalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := replayed.persist(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if e.Kind == events.KindSessionTurnOutput {
			if e.Envelope.Corr.CausationID != native.ID || e.Envelope.ParentID != native.ParentID || !e.At.Equal(native.Time) || e.Envelope.Actor.Verified != "token" || !e.Envelope.Class.PII {
				t.Fatalf("journal replay lost origin: %+v", e)
			}
			return
		}
	}
	t.Fatal("replayed output missing")
}

func TestDerivedOutputIgnoresTelemetryOriginAndClearsTurnFloors(t *testing.T) {
	svc, output := outputHarness(t, nil)
	native := nativeOriginExample()
	native.ID = "first-delta"
	native.Kind = runtimeevents.KindAgentDelta
	native.Class.Sensitivity = "private"
	native.Payload = json.RawMessage(`{"content":"unfinished output"}`)
	output.observeRuntime(native)
	telemetry := native
	telemetry.ID = "unrelated-telemetry"
	telemetry.Kind = runtimeevents.EventKind("process.telemetry")
	telemetry.TurnID = ""
	telemetry.Class = nil
	telemetry.Payload = nil
	output.observeRuntime(telemetry)
	output.flush()
	second := native
	second.ID = "second-delta"
	second.TurnID = "second-turn"
	second.Class = &runtimeevents.Class{Sensitivity: "public"}
	second.Payload = json.RawMessage(`{"content":"second output","phase":"final"}`)
	output.observeRuntime(second)
	second.ID = "second-terminal"
	second.Kind = runtimeevents.KindTurnCompleted
	second.Payload = nil
	output.observeRuntime(second)
	rows, err := svc.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range rows {
		if e.Kind != events.KindSessionTurnOutput {
			continue
		}
		seen++
		switch e.Envelope.TurnID {
		case "native-turn":
			if e.Envelope.Corr.CausationID != "first-delta" || e.Envelope.Class.Sensitivity != "private" || !e.Envelope.Class.PII {
				t.Fatalf("telemetry replaced output provenance: %+v", e)
			}
		case "second-turn":
			if e.Envelope.Corr.CausationID != "second-terminal" || e.Envelope.Class.Sensitivity != "public" || e.Envelope.Class.PII {
				t.Fatalf("previous turn leaked into next output: %+v", e)
			}
		default:
			t.Fatalf("unexpected output origin: %+v", e)
		}
	}
	if seen != 2 {
		t.Fatalf("missing output: %d", seen)
	}
}

func TestDerivedOverlappingTurnsKeepTheirOwnFloors(t *testing.T) {
	for _, lateSecret := range []bool{false, true} {
		for _, routed := range []bool{false, true} {
			for _, finishBFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("late-secret=%t/routed=%t/B-first=%t", lateSecret, routed, finishBFirst), func(t *testing.T) {
					var route *launchprofile.Route
					if routed {
						route = &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}
					}
					svc, output := outputHarness(t, route)
					emit := func(turn, id string, kind runtimeevents.EventKind, sensitivity, text string) {
						ev := nativeOriginExample()
						ev.TurnID, ev.ID, ev.Kind = turn, id, kind
						ev.Class = &runtimeevents.Class{Sensitivity: sensitivity}
						ev.Payload = nil
						if text != "" {
							ev.Payload, _ = json.Marshal(map[string]string{"content": text, "phase": "final"})
						}
						output.observeRuntime(ev)
					}
					firstClass := "secret"
					if lateSecret {
						firstClass = "public"
					}
					emit("A", "A-start", runtimeevents.KindTurnStarted, firstClass, "")
					if !lateSecret {
						emit("A", "A-delta", runtimeevents.KindAgentDelta, "", "hidden-marker")
					}
					emit("B", "B-start", runtimeevents.KindTurnStarted, "public", "")
					if lateSecret {
						emit("A", "A-delta", runtimeevents.KindAgentDelta, "secret", "hidden-marker")
					}
					if !finishBFirst {
						emit("A", "A-end", runtimeevents.KindTurnCompleted, "", "")
					}
					emit("B", "B-delta", runtimeevents.KindAgentDelta, "public", "visible-output")
					emit("B", "B-end", runtimeevents.KindTurnCompleted, "", "")
					if finishBFirst {
						emit("A", "A-end", runtimeevents.KindTurnCompleted, "", "")
					}
					rows, err := svc.Store.EventsSince(0)
					if err != nil {
						t.Fatal(err)
					}
					seen := map[string]bool{}
					for _, e := range rows {
						if e.Kind != events.KindSessionTurnOutput {
							continue
						}
						var payload events.TurnOutputEvent
						if err := json.Unmarshal([]byte(e.PayloadJSON), &payload); err != nil {
							t.Fatal(err)
						}
						seen[payload.TurnID] = true
						if e.Envelope.Corr.CausationID != payload.TurnID+"-end" {
							t.Fatalf("wrong turn origin: %+v", e)
						}
						if payload.TurnID == "A" && (payload.Reason != "secret_withheld" || payload.Text != "" || strings.Contains(e.PayloadJSON, "hidden-marker")) {
							t.Fatalf("secret leaked: %+v", payload)
						}
						if payload.TurnID == "B" && (payload.Reason != "" || e.Envelope.Class.Sensitivity != "public") {
							t.Fatalf("public turn inherited secret floor: %+v", e)
						}
						if payload.MessageID != "" {
							staged, err := svc.Store.StagedTurnOutput(context.Background(), payload.MessageID)
							if err != nil || strings.Contains(string(staged.Payload), "hidden-marker") {
								t.Fatalf("staged secret: %v", err)
							}
						}
					}
					if !seen["A"] || !seen["B"] {
						t.Fatalf("missing outputs: %v", seen)
					}
					if len(output.runtimeOrigins) != 0 {
						t.Fatal("completed origins retained")
					}
				})
			}
		}
	}

}

type rejectedDerivedOutputBus struct{ events.Bus }

func (b rejectedDerivedOutputBus) Publish(ctx context.Context, e events.Event) error {
	if e.Kind == events.KindSessionTurnOutput {
		return errors.New("injected output failure")
	}
	return b.Bus.Publish(ctx, e)
}

func TestDerivedUntaggedSecretDoesNotUseStaleOrigin(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, failEvent := range []bool{false, true} {
			t.Run(fmt.Sprintf("routed=%t/retry=%t", routed, failEvent), func(t *testing.T) {
				var route *launchprofile.Route
				if routed {
					route = &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}
				}
				svc, output := outputHarness(t, route)
				if failEvent {
					svc.Bus = rejectedDerivedOutputBus{svc.Bus}
				}
				emit := func(turn string, kind runtimeevents.EventKind, class, text string) {
					ev := nativeOriginExample()
					ev.TurnID = turn
					ev.Kind = kind
					ev.Class = &runtimeevents.Class{Sensitivity: class, PII: class == "secret"}
					ev.Payload = nil
					if text != "" {
						ev.Payload, _ = json.Marshal(map[string]string{"content": text, "phase": "final"})
					}
					output.observeRuntime(ev)
				}
				emit("", runtimeevents.KindTurnStarted, "public", "")
				if len(output.runtimeOrigins) != 0 {
					t.Fatal("untagged start invented origin identity")
				}
				emit("T1", runtimeevents.KindAgentDelta, "public", "ok")
				emit("T1", runtimeevents.KindTurnCompleted, "", "")
				emit("", runtimeevents.KindAgentDelta, "secret", "hidden-untagged-marker")
				emit("", runtimeevents.KindTurnCompleted, "", "")
				svc.stopOutputRetries()
				rows, err := svc.Store.EventsSince(0)
				if err != nil {
					t.Fatal(err)
				}
				withheld := false
				for _, e := range rows {
					if strings.Contains(e.PayloadJSON, "hidden-untagged-marker") {
						t.Fatal("untagged secret reached event")
					}
					if e.Kind != events.KindSessionTurnOutput {
						continue
					}
					var payload events.TurnOutputEvent
					if err := json.Unmarshal([]byte(e.PayloadJSON), &payload); err != nil {
						t.Fatal(err)
					}
					if payload.Reason == "secret_withheld" {
						withheld = true
						if !e.Envelope.Class.PII {
							t.Fatal("untagged PII floor lost")
						}
						if payload.MessageID != "" {
							staged, err := svc.Store.StagedTurnOutput(context.Background(), payload.MessageID)
							if err != nil || strings.Contains(string(staged.Payload), "hidden-untagged-marker") {
								t.Fatal("untagged secret reached stage", err)
							}
						}
					}
				}
				ids, err := svc.Store.PendingTurnOutputRetries("", 128)
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range ids {
					raw, err := svc.Store.ReadTurnOutputRetry(id)
					if err != nil || strings.Contains(string(raw), "hidden-untagged-marker") {
						t.Fatal("untagged secret reached journal", err)
					}
					if strings.Contains(string(raw), "secret_withheld") {
						withheld = true
					}
				}
				if !withheld {
					t.Fatal("untagged secret was not withheld")
				}
			})
		}
	}
}

func TestDerivedOriginEvictionCarriesFloor(t *testing.T) {
	svc, output := outputHarness(t, nil)
	for i := 0; i < 70; i++ {
		ev := nativeOriginExample()
		ev.TurnID = fmt.Sprintf("turn-%d", i)
		ev.Kind = runtimeevents.KindTurnStarted
		ev.Class = &runtimeevents.Class{Sensitivity: "public"}
		if i == 0 {
			ev.Class = &runtimeevents.Class{Sensitivity: "secret", PII: true}
		}
		output.observeRuntime(ev)
	}
	if len(output.runtimeOrigins) > 64 || len(output.runtimeOriginOrder) > 64 {
		t.Fatal("origin tracking is unbounded")
	}
	ev := nativeOriginExample()
	ev.TurnID = "turn-69"
	ev.Kind = runtimeevents.KindAgentDelta
	ev.Class = nil
	ev.Payload = json.RawMessage(`{"content":"hidden-eviction-marker","phase":"final"}`)
	output.observeRuntime(ev)
	ev.Kind = runtimeevents.KindTurnCompleted
	ev.Payload = nil
	output.observeRuntime(ev)
	payloads := outputEvents(t, svc)
	if len(payloads) != 1 || payloads[0].Reason != "secret_withheld" || payloads[0].Text != "" {
		t.Fatalf("eviction discarded conservative floor: %+v", payloads)
	}
}
