package main

import (
	"strings"
	"testing"
)

func TestPeekDigest(t *testing.T) {
	log := strings.Join([]string{
		`not json`,
		`{"type":"system","subtype":"init","model":"m1","mcp_servers":[{"name":"tether","status":"connected"}]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"},{"type":"tool_use","name":"Bash","input":{"command":"echo GH_TOKEN=abc123secret"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok sk-abcdefghijkl","is_error":false}]}}`,
		`{"type":"result","subtype":"success","num_turns":3,"total_cost_usd":0.5,"result":"done"}`,
	}, "\n")
	got := peekDigest([]byte(log), 0)
	if len(got) != 5 {
		t.Fatalf("lines=%d: %q", len(got), got)
	}
	all := strings.Join(got, "\n")
	for _, bad := range []string{"abc123secret", "sk-abcdefghijkl"} {
		if strings.Contains(all, bad) {
			t.Fatalf("secret %q leaked: %s", bad, all)
		}
	}
	for _, want := range []string{"init   model=m1 mcp=[tether=connected]", "say    hello", "tool   Bash", "FINAL  ok turns=3 cost=$0.5000 done"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %s", want, all)
		}
	}
	if tail := peekDigest([]byte(log), 2); len(tail) != 2 || !strings.HasPrefix(tail[1], "FINAL") {
		t.Fatalf("tail: %q", tail)
	}
}
