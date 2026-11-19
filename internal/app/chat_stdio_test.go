package app

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// A worker Claude session's chat tap: created by the first backend, fed by accepted stdout frames (split anywhere),
// noted by SendTurn before the write, closed when the worker reports the session exited.
func TestWorkerClaudeStdioTap(t *testing.T) {
	s := permissionWorkerHarness(t)
	raw, _ := json.Marshal(launch.Plan{ProviderBrand: "claude", RuntimeKind: config.RuntimeKindStreamingStdio})
	if _, err := s.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id='s1'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	if s.stdioTap("s1") != nil {
		t.Fatal("a worker session has a tap before any backend asked for one")
	}
	if _, err := s.ClaudeStdioBackend("s1", claudestdio.Config{}); err != nil {
		t.Fatal(err)
	}
	tap := s.stdioTap("s1")
	if tap == nil {
		t.Fatal("no tap")
	}
	var lines []string
	var notes, failed []claudestdio.InputNote
	tap.Subscribe(claudestdio.Subscriber{
		Line:        func(l []byte) { lines = append(lines, string(l)) },
		Input:       func(n claudestdio.InputNote) { notes = append(notes, n) },
		InputFailed: func(n claudestdio.InputNote) { failed = append(failed, n) },
	})
	frames := []string{`{"type":"system","subtype":"init"}` + "\n" + `{"type":"res`, `ult","subtype":"success"}` + "\n"}
	for i, f := range frames {
		if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: int64(i + 1), Stream: "stdout", Data: []byte(f)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 3, Stream: "stderr", Data: []byte("{\"x\":1}\n")})
	if want := []string{`{"type":"system","subtype":"init"}`, `{"type":"result","subtype":"success"}`}; !reflect.DeepEqual(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}

	// The worker is not connected, so the write fails: the frame was noted first, then reported failed.
	if err := s.SendTurn(context.Background(), "s1", "hello"); err == nil {
		t.Fatal("SendTurn to a disconnected worker succeeded")
	}
	if len(notes) != 1 || notes[0].Text != "hello" || notes[0].From != stdioFromSendTurn || len(failed) != 1 {
		t.Errorf("notes = %+v, failed = %+v", notes, failed)
	}

	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "state", Session: "s1", State: "exited"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tap.Done():
	default:
		t.Error("tap not closed when the session exited")
	}
	if s.stdioTap("s1") != nil {
		t.Error("tap kept after exit")
	}
}

func TestClaudeStdioBackendRefusesOtherSessions(t *testing.T) {
	s := permissionWorkerHarness(t) // a codex worker session
	if _, err := s.ClaudeStdioBackend("s1", claudestdio.Config{}); err == nil {
		t.Error("backend for a codex session")
	}
	if _, err := s.ClaudeStdioBackend("nope", claudestdio.Config{}); err == nil {
		t.Error("backend for an unknown session")
	}
}

// A local Claude streaming-stdio launch gets a tap on its output Fanout, next to any Fanout already set; other
// sessions get none.
func TestInstallStdioTap(t *testing.T) {
	s := &Service{}
	stdio := strictNativeCoauthorRuntime{caps: agentsessions.Capabilities{StreamingStdio: true}}
	var other bytes.Buffer
	opts := agentsessions.StartOptions{Fanout: &other}
	tap := s.installStdioTap("s1", "claude", stdio, &opts)
	if tap == nil || s.stdioTap("s1") != tap {
		t.Fatal("no tap installed")
	}
	var lines []string
	tap.Subscribe(claudestdio.Subscriber{Line: func(l []byte) { lines = append(lines, string(l)) }})
	_, _ = opts.Fanout.Write([]byte("{\"type\":\"system\",\"subtype\":\"init\"}\n"))
	if len(lines) != 1 || other.Len() == 0 {
		t.Errorf("tap lines %q, existing fanout %d bytes", lines, other.Len())
	}
	s.dropStdioTap("s1", tap)
	if s.stdioTap("s1") != nil {
		t.Error("tap kept after drop")
	}
	for _, tc := range []struct {
		brand string
		rt    agentsessions.Runtime
	}{
		{"codex", stdio},
		{"claude", strictNativeCoauthorRuntime{caps: agentsessions.Capabilities{PTY: true}}},
	} {
		o := agentsessions.StartOptions{}
		if s.installStdioTap("s2", tc.brand, tc.rt, &o) != nil || o.Fanout != nil {
			t.Errorf("%s %+v: tap installed", tc.brand, tc.rt.Caps())
		}
	}
}
