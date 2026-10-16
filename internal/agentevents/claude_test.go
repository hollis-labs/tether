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
		"SubagentStop":             K(runtimeevents.KindAgentSubagentStop),
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
		if e.IsDir() {
			continue
		}
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
	realEntries, _ := os.ReadDir(filepath.Join("testdata", "claude", "real"))
	for _, e := range realEntries {
		raw, _ := os.ReadFile(filepath.Join("testdata", "claude", "real", e.Name()))
		obs, err := ClaudeHook(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range obs {
			b, _ := json.Marshal(o.Payload)
			all.Write(b)
		}
	}
	for _, secret := range []string{"hello from the capture", "List the files", "directory has one entry", "hunter2", "SECRET", ".env", "secret file", "rm -rf", "Which option", "needs your permission"} {
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

// Real payloads, captured from a throwaway `claude -p` session (Claude Code 2.1.301) by a hook that only wrote its stdin to a
// file, then scrubbed of paths, ids and the user name. Every event the session fired is here except PermissionRequest,
// PermissionDenied, Notification and TeammateIdle, which a short non-interactive run does not trigger: those stay hand-written.
func TestClaudeHookMappingOfRealCapturedPayloads(t *testing.T) {
	type want struct {
		kind    runtimeevents.EventKind
		turn    string
		payload string
	}
	for fixture, w := range map[string]want{
		"SessionStart":             {runtimeevents.KindSessionReady, "", `{"source":"startup"}`},
		"UserPromptSubmit":         {runtimeevents.KindTurnStarted, "prompt-1", `{"prompt_bytes":149}`},
		"PreToolUse-read":          {runtimeevents.KindAgentToolUse, "prompt-1", `{"input_bytes":37,"tool":"Read","tool_use_id":"toolu_1"}`},
		"PostToolUse-read":         {runtimeevents.KindAgentToolResult, "prompt-1", `{"duration_ms":121,"ok":true,"result_bytes":170,"tool":"Read","tool_use_id":"toolu_1"}`},
		"PreToolUse-agent":         {runtimeevents.KindAgentToolUse, "prompt-1", ""},
		"SubagentStart":            {runtimeevents.KindAgentSubagentSpawn, "prompt-1", `{"agent_type":"Explore","subagent_id":"agent-1"}`},
		"PreToolUse-bash-subagent": {runtimeevents.KindAgentToolUse, "prompt-1", ""},
		"SubagentStop":             {runtimeevents.KindAgentSubagentStop, "prompt-1", `{"agent_type":"Explore","outcome":"completed","subagent_id":"agent-1"}`},
		"Stop":                     {runtimeevents.KindTurnCompleted, "prompt-1", ""},
		"SessionEnd":               {runtimeevents.KindProcessExited, "prompt-1", ""},
	} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "claude", "real", fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			obs, err := ClaudeHook(raw)
			if err != nil || len(obs) == 0 {
				t.Fatalf("obs=%v err=%v", obs, err)
			}
			o := obs[0]
			if o.Kind != w.kind || o.TurnID != w.turn || o.Tier != Exact {
				t.Fatalf("kind %q turn %q tier %q, want %q %q exact", o.Kind, o.TurnID, o.Tier, w.kind, w.turn)
			}
			if w.payload != "" {
				if got, _ := json.Marshal(o.Payload); string(got) != w.payload {
					t.Fatalf("payload = %s, want %s", got, w.payload)
				}
			}
		})
	}
	// A tool call made inside a subagent says so; the session's own does not.
	inner, _ := os.ReadFile(filepath.Join("testdata", "claude", "real", "PreToolUse-bash-subagent.json"))
	outer, _ := os.ReadFile(filepath.Join("testdata", "claude", "real", "PreToolUse-bash.json"))
	oi, _ := ClaudeHook(inner)
	oo, _ := ClaudeHook(outer)
	gi, _ := json.Marshal(oi[0].Payload)
	outerJSON, _ := json.Marshal(oo[0].Payload)
	if !strings.Contains(string(gi), `"subagent_id":"agent-`) || strings.Contains(string(outerJSON), "subagent_id") {
		t.Fatalf("subagent attribution: inner %s outer %s", gi, outerJSON)
	}
}
