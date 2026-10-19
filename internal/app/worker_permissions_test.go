package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func permissionWorkerHarness(t *testing.T) *Service {
	t.Helper()
	s, _ := outputHarness(t, nil)
	if _, err := s.Store.DB().Exec(`UPDATE sessions SET worker_id='worker',workspace=?,state='running' WHERE id='s1'`, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(launch.Plan{ProviderBrand: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio})
	if _, err := s.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id='s1'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	s.Workers = workerhub.New(s.Store.DB())
	return s
}
func permissionMarker(t *testing.T, kind runtimeevents.EventKind, turn, id string) []byte {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"request_id": id, "method": "item/commandExecution/requestApproval", "params": "private-content"})
	raw, err := json.Marshal(map[string]any{"type": "tether.runtime_event", "event": runtimeevents.Event{Kind: kind, TurnID: turn, SessionID: "forged-session", Payload: payload}})
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}
func permissionSnapshot(t *testing.T, s *Service) []store.EnvironmentRequest {
	t.Helper()
	base, err := s.Store.EnvironmentSnapshot(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	return base.Requests
}
func TestWorkerPermissionForwardingRestartOverlapAndTerminal(t *testing.T) {
	s := permissionWorkerHarness(t)
	seq := int64(0)
	ingest := func(stream string, data []byte) {
		t.Helper()
		seq++
		if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: seq, Stream: stream, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	line := permissionMarker(t, runtimeevents.KindAgentPermissionRequested, "turn", "one")
	split := len(line) / 2
	ingest("stdout", line[:split])
	if got := permissionSnapshot(t, s); len(got) != 0 {
		t.Fatal("partial request established state", got)
	}
	ingest("stderr", []byte("interleaved stderr\n"))
	// A fresh service has no in-memory partial-line state.
	s = &Service{Store: s.Store, Bus: events.NewBus(events.BusOptions{Persister: s.Store}), Workers: workerhub.New(s.Store.DB())}
	ingest("stdout", line[split:])
	ingest("stdout", permissionMarker(t, runtimeevents.KindAgentPermissionRequested, "turn", "two"))
	ingest("stdout", permissionMarker(t, runtimeevents.KindAgentPermissionResolved, "turn", "one"))
	got := permissionSnapshot(t, s)
	if len(got) != 2 || got[0].Open || !got[1].Open || got[0].SourceSequence <= 1<<32 {
		t.Fatalf("overlap/sequence %+v", got)
	}
	if err := s.Store.UpdateSessionState("s1", "orphaned", 0, nil); err != nil {
		t.Fatal(err)
	}
	ingest("stdout", permissionMarker(t, runtimeevents.KindTurnCompleted, "other", ""))
	if got = permissionSnapshot(t, s); !got[1].Open {
		t.Fatal("disconnect/other turn cleared request", got)
	}
	ingest("stdout", permissionMarker(t, runtimeevents.KindTurnCompleted, "turn", ""))
	if got = permissionSnapshot(t, s); got[1].Open {
		t.Fatal("terminal did not resolve", got)
	}
	all, err := s.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range all {
		if ev.Kind != string(runtimeevents.KindAgentPermissionRequested) && ev.Kind != string(runtimeevents.KindAgentPermissionResolved) {
			continue
		}
		count++
		if ev.SessionID != "s1" || strings.Contains(ev.PayloadJSON, "private-content") || strings.Contains(ev.PayloadJSON, "forged-session") {
			t.Fatalf("unsafe metadata %+v", ev)
		}
	}
	if count != 3 {
		t.Fatalf("native count %d", count)
	}
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: seq, Stream: "stdout", Data: line}); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("foreign", workerwire.Frame{T: "output", Session: "s1", Seq: seq + 1, Stream: "stdout", Data: line}); err == nil {
		t.Fatal("foreign worker accepted")
	}
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: seq + 2, Stream: "stdout", Data: line}); err == nil {
		t.Fatal("gap accepted")
	}
	state, err := s.Store.WorkerPermissionParser(context.Background(), "s1")
	if err != nil || strings.Contains(state.Spans, "private") {
		t.Fatal("parser persisted content", state, err)
	}
}
func TestWorkerPermissionFailureReplayAndUnknownSource(t *testing.T) {
	s := permissionWorkerHarness(t)
	frame := workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: permissionMarker(t, runtimeevents.KindAgentPermissionRequested, "turn", "one")}
	if _, err := s.Store.DB().Exec(`CREATE TRIGGER reject_request BEFORE INSERT ON events WHEN NEW.kind='session.request_status' BEGIN SELECT RAISE(FAIL,'retry'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("worker", frame); err == nil {
		t.Fatal("persistence failure lost")
	}
	if s.Workers.NextSeq("s1") != 1 {
		t.Fatal("failed observation advanced cursor")
	}
	if _, err := s.Store.DB().Exec(`DROP TRIGGER reject_request`); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("worker", frame); err != nil {
		t.Fatal(err)
	}
	if got := permissionSnapshot(t, s); len(got) != 1 || !got[0].Open {
		t.Fatal(got)
	}
	other := permissionWorkerHarness(t)
	if _, err := other.Store.DB().Exec(`UPDATE launch_plans SET plan_json='{"provider_brand":"unknown"}' WHERE session_id='s1'`); err != nil {
		t.Fatal(err)
	}
	if err := other.ingestWorkerFrame("worker", frame); err != nil {
		t.Fatal(err)
	}
	if got := permissionSnapshot(t, other); len(got) != 0 {
		t.Fatal("unknown source established known state", got)
	}
}

func TestWorkerPermissionBatchOversizeAndHealth(t *testing.T) {
	s := permissionWorkerHarness(t)
	seq := int64(0)
	send := func(data []byte) {
		t.Helper()
		seq++
		if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: seq, Stream: "stdout", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	health := func(want *bool) {
		t.Helper()
		h, err := s.SessionHealthObservations(context.Background(), "s1")
		if err != nil {
			t.Fatal(err)
		}
		if (h.PendingApproval == nil) != (want == nil) || (want != nil && *h.PendingApproval != *want) || h.PendingQuestion != nil {
			t.Fatalf("health %+v wanted %v", h, want)
		}
	}
	health(nil)
	send([]byte(strings.Repeat("x", workerPermissionLineLimit+1)))
	// The continuation of an oversized line is discarded, then normal framing resumes.
	open := permissionMarker(t, runtimeevents.KindAgentPermissionRequested, "turn", "one")
	send(append([]byte("trailing\n"), open...))
	yes, no := true, false
	health(&yes)
	// Two complete markers in one output frame must have distinct ordered coordinates.
	resolution := permissionMarker(t, runtimeevents.KindAgentPermissionResolved, "turn", "one")
	open2 := permissionMarker(t, runtimeevents.KindAgentPermissionRequested, "turn", "two")
	send(append(resolution, open2...))
	got := permissionSnapshot(t, s)
	if len(got) != 2 || got[0].Open || !got[1].Open || got[0].SourceSequence != uint64(seq)<<32|uint64(len(resolution)) || got[1].SourceSequence != uint64(seq)<<32|uint64(len(resolution)+len(open2)) {
		t.Fatalf("batch ordering %+v", got)
	}
	health(&yes)
	send(permissionMarker(t, runtimeevents.KindAgentPermissionResolved, "turn", "two"))
	health(&no)
	unknown := permissionWorkerHarness(t)
	if _, err := unknown.Store.DB().Exec(`UPDATE launch_plans SET plan_json='{}' WHERE session_id='s1'`); err != nil {
		t.Fatal(err)
	}
	if err := unknown.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: open}); err != nil {
		t.Fatal(err)
	}
	h, err := unknown.SessionHealthObservations(context.Background(), "s1")
	if err != nil || h.PendingApproval != nil || h.PendingQuestion != nil {
		t.Fatal("unsupported health must stay unknown", h, err)
	}
}
