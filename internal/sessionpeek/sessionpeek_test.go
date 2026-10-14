package sessionpeek

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPeekDigest(t *testing.T) {
	log := strings.Join([]string{
		`not json`,
		`{"type":"system","subtype":"init","model":"m1","mcp_servers":[{"name":"tether","status":"connected"}]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"},{"type":"tool_use","name":"Bash","input":{"command":"echo GH_TOKEN=abc123secret"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok sk-abcdefghijkl","is_error":false}]}}`,
		`{"type":"result","subtype":"success","num_turns":3,"total_cost_usd":0.5,"result":"done"}`,
	}, "\n")
	got := Digest([]byte(log), 0)
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
	if tail := Digest([]byte(log), 2); len(tail) != 2 || !strings.HasPrefix(tail[1], "FINAL") {
		t.Fatalf("tail: %q", tail)
	}
}

func TestTruncRedactsMoreSecretsAndKeepsRunesWhole(t *testing.T) {
	for _, in := range []string{
		"key AKIAABCDEFGHIJKLMNOP end",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----",
		"Cookie: session=abcdef123456",
	} {
		out := trunc(in, 500)
		if strings.Contains(out, "AKIAABC") || strings.Contains(out, "MIIabc") || strings.Contains(out, "abcdef123456") {
			t.Errorf("secret survived: %q -> %q", in, out)
		}
	}
	out := trunc("ééééé", 3)
	if !utf8.ValidString(out) {
		t.Errorf("truncation split a rune: %q", out)
	}
}
