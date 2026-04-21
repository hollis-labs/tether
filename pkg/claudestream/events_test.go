package claudestream

import (
	"strings"
	"testing"
)

func TestParse_Empty(t *testing.T) {
	events, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty input, got %d", len(events))
	}
}

func TestParse_SystemInitYieldsSessionID(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","cwd":"/tmp","session_id":"abc","tools":["Read","Write"],"model":"claude-opus-4-6"}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != KindSessionID || events[0].SessionID != "abc" {
		t.Fatalf("expected one session_id=abc event, got %+v", events)
	}
}

func TestParse_SystemNonInitIgnored(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"api_retry","session_id":"abc"}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for non-init system, got %d", len(events))
	}
}

func TestParse_SystemInitMissingSessionID(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","cwd":"/tmp"}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for init without session_id, got %d", len(events))
	}
}

func TestParse_AssistantText(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello there!"}]}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != KindDelta || events[0].Text != "Hello there!" {
		t.Fatalf("expected one delta event 'Hello there!', got %+v", events)
	}
}

func TestParse_AssistantToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_123","name":"Read","input":{"file_path":"/tmp/test.txt"}}]}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != KindToolUse || ev.ToolUse == nil {
		t.Fatalf("expected tool_use event, got %+v", ev)
	}
	if ev.ToolUse.ID != "tu_123" || ev.ToolUse.Name != "Read" {
		t.Errorf("tool id/name wrong: %+v", ev.ToolUse)
	}
	if fp, ok := ev.ToolUse.Input["file_path"]; !ok || fp != "/tmp/test.txt" {
		t.Errorf("tool input wrong: %v", ev.ToolUse.Input)
	}
}

func TestParse_AssistantMixed(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Reading file."},{"type":"tool_use","id":"tu_456","name":"Bash","input":{"command":"ls"}}]}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Kind != KindDelta || events[1].Kind != KindToolUse {
		t.Errorf("event kinds wrong: %+v", events)
	}
}

func TestParse_ResultSuccess(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"Done.","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":500,"cache_read_input_tokens":0}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected usage+done (2), got %d", len(events))
	}
	if events[0].Kind != KindUsage || events[0].Usage == nil {
		t.Fatalf("events[0] not usage: %+v", events[0])
	}
	u := events[0].Usage
	if u.InputTokens != 100 || u.OutputTokens != 20 || u.CacheCreationTokens != 500 || u.StopReason != "end_turn" {
		t.Errorf("usage fields wrong: %+v", u)
	}
	if events[1].Kind != KindDone {
		t.Errorf("events[1] not done: %+v", events[1])
	}
}

func TestParse_ResultError(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"error","is_error":true,"result":"Something went wrong"}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != KindError || events[0].ErrorMsg != "Something went wrong" {
		t.Fatalf("expected error event, got %+v", events)
	}
}

func TestParse_TopLevelError(t *testing.T) {
	line := []byte(`{"type":"error","error":{"message":"API key invalid"}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != KindError || events[0].ErrorMsg != "API key invalid" {
		t.Fatalf("expected error event, got %+v", events)
	}
}

func TestParse_RateLimitIgnored(t *testing.T) {
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for rate_limit_event, got %d", len(events))
	}
}

func TestParse_UnknownTypeIgnored(t *testing.T) {
	line := []byte(`{"type":"future_event","data":"something"}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}

func TestParse_InvalidJSONErrors(t *testing.T) {
	_, err := Parse([]byte(`not valid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestScanner_DrainsMultiEventLines(t *testing.T) {
	// A line that produces two events (assistant with text + tool_use).
	input := strings.NewReader(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}
{"type":"result","subtype":"success","is_error":false,"result":"ok","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}
`)
	s := NewScanner(input)
	var kinds []Kind
	for {
		ev, ok, err := s.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			break
		}
		kinds = append(kinds, ev.Kind)
	}
	want := []Kind{KindDelta, KindToolUse, KindUsage, KindDone}
	if len(kinds) != len(want) {
		t.Fatalf("expected %v, got %v", want, kinds)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Errorf("kinds[%d]: got %q, want %q", i, kinds[i], k)
		}
	}
}

func TestScanner_SkipsBlankAndIgnoredLines(t *testing.T) {
	input := strings.NewReader(`{"type":"rate_limit_event"}

{"type":"system","subtype":"init","session_id":"s1"}
`)
	s := NewScanner(input)
	ev, ok, err := s.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || ev.Kind != KindSessionID || ev.SessionID != "s1" {
		t.Fatalf("expected session_id=s1, got %+v", ev)
	}
	if _, ok, err := s.Next(); ok || err != nil {
		t.Fatalf("expected EOF, got ok=%v err=%v", ok, err)
	}
}

func TestParse_UIPrompt(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"ui_prompt","input":{"kind":"yes_no","title":"Continue?","default":"yes"}}]}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != KindUIPrompt {
		t.Errorf("want KindUIPrompt, got %q", ev.Kind)
	}
	if ev.UIPrompt == nil {
		t.Fatal("UIPrompt is nil")
	}
	if ev.UIPrompt.Kind != "yes_no" {
		t.Errorf("want kind yes_no, got %q", ev.UIPrompt.Kind)
	}
	if ev.UIPrompt.Title != "Continue?" {
		t.Errorf("want title Continue?, got %q", ev.UIPrompt.Title)
	}
	if ev.UIPrompt.ToolUseID != "toolu_01" {
		t.Errorf("want ToolUseID toolu_01, got %q", ev.UIPrompt.ToolUseID)
	}
}

func TestParse_UIPrompt_NotForwardedAsToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02","name":"ui_prompt","input":{"kind":"yes_no","title":"Ok?"}}]}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == KindToolUse {
			t.Error("ui_prompt must not be emitted as KindToolUse")
		}
	}
}
