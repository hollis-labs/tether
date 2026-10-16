package agentevents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

// The fixtures in testdata/claude are written from the documented hook payload fields and the shapes tether-mail's pulse hook
// already reads; they are not captured from a live session.
func TestClaudeHookMapping(t *testing.T) {
	K := func(ks ...runtimeevents.EventKind) []runtimeevents.EventKind { return ks }
	for fixture, want := range map[string][]runtimeevents.EventKind{
		"SessionStart":             K(runtimeevents.KindSessionReady),
		"UserPromptSubmit":         K(runtimeevents.KindTurnStarted, runtimeevents.KindSessionProcessing),
		"PreToolUse":               K(runtimeevents.KindAgentToolUse),
		"PostToolUse":              K(runtimeevents.KindAgentToolResult),
		"PostToolUseFailure":       K(runtimeevents.KindAgentToolResult),
		"PermissionRequest":        K(runtimeevents.KindAgentPermissionRequested),
		"PermissionDenied":         K(runtimeevents.KindAgentPermissionDenied),
		"Notification-permission":  K(runtimeevents.KindAgentPermissionRequested),
		"Notification-elicitation": K(runtimeevents.KindAgentPermissionRequested),
		"Notification-idle":        K(runtimeevents.KindSessionIdle),
		"Notification-other":       nil,
		"SubagentStart":            K(runtimeevents.KindAgentSubagentSpawn),
		"SubagentStop":             K(KindAgentSubagentStop),
		"TeammateIdle":             nil,
		"Stop":                     K(runtimeevents.KindTurnCompleted, runtimeevents.KindSessionIdle),
		"SessionEnd":               K(runtimeevents.KindProcessExited),
		"Unknown":                  nil,
	} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "claude", fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			obs, err := ClaudeHook(raw)
			if err != nil {
				t.Fatal(err)
			}
			var got []runtimeevents.EventKind
			for _, o := range obs {
				got = append(got, o.Kind)
				if o.Tier != Exact || o.Channel != runtimeevents.ChannelHook {
					t.Fatalf("%s: tier %q channel %q, want exact hook", o.Kind, o.Tier, o.Channel)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("kinds = %v, want %v", got, want)
			}
			// Every mapped observation must become a valid, spoolable v1.1 event.
			r := newRecorder(t, t.TempDir(), "sess-fixture")
			for _, o := range obs {
				if _, err := r.Record(context.Background(), o); err != nil {
					t.Fatalf("%s does not record: %v", o.Kind, err)
				}
			}
		})
	}
}

func TestClaudeHookPayloadsCarryNoContent(t *testing.T) {
	var all strings.Builder
	entries, _ := os.ReadDir(filepath.Join("testdata", "claude"))
	for _, e := range entries {
		raw, _ := os.ReadFile(filepath.Join("testdata", "claude", e.Name()))
		obs, err := ClaudeHook(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range obs {
			b, _ := json.Marshal(o.Payload)
			all.Write(b)
		}
	}
	for _, secret := range []string{"hunter2", "SECRET", ".env", "secret file", "rm -rf", "Which option", "needs your permission"} {
		if strings.Contains(all.String(), secret) {
			t.Fatalf("an observation payload carries content %q: %s", secret, all.String())
		}
	}
	// What is recorded: names, ids and sizes.
	raw, _ := os.ReadFile(filepath.Join("testdata", "claude", "PostToolUse.json"))
	obs, _ := ClaudeHook(raw)
	if got, _ := json.Marshal(obs[0].Payload); string(got) != `{"ok":true,"result_bytes":28,"tool":"Read","tool_use_id":"toolu_1"}` {
		t.Fatalf("PostToolUse payload = %s", got)
	}
}

func TestClaudeHookRefusesPayloadsThatAreNotHooks(t *testing.T) {
	for _, bad := range []string{``, `not json`, `{}`, `{"session_id":"c1"}`} {
		if _, err := ClaudeHook([]byte(bad)); err == nil {
			t.Fatalf("payload %q accepted", bad)
		}
	}
	if ClaudeSessionID([]byte(`{"session_id":"c1"}`)) != "c1" || ClaudeSessionID([]byte(`x`)) != "" {
		t.Fatal("ClaudeSessionID")
	}
}

func TestSubagentStopIsTheRuledKindWithItsRequiredPayload(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join("testdata", "claude", "SubagentStop.json"))
	obs, _ := ClaudeHook(raw)
	if len(obs) != 1 || obs[0].Kind != "agent.subagent_stop" {
		t.Fatalf("SubagentStop = %+v", obs)
	}
	if got, _ := json.Marshal(obs[0].Payload); string(got) != `{"agent_type":"Explore","outcome":"completed","subagent_id":"a1"}` {
		t.Fatalf("payload = %s", got)
	}
	// It records as a valid event, and the adapter can set ParentID to the spawn event.
	obs[0].ParentID = "evt_spawn"
	ev, err := newRecorder(t, t.TempDir(), "sess-sub").Record(context.Background(), obs[0])
	if err != nil || ev.ParentID != "evt_spawn" {
		t.Fatalf("recorded = %+v err=%v", ev, err)
	}
	start, _ := os.ReadFile(filepath.Join("testdata", "claude", "SubagentStart.json"))
	spawn, _ := ClaudeHook(start)
	if got, _ := json.Marshal(spawn[0].Payload); string(got) != `{"agent_type":"Explore","subagent_id":"a1"}` {
		t.Fatalf("spawn payload = %s", got)
	}
}
