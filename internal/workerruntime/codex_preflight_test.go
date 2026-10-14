package workerruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexCredentialPreflight(t *testing.T) {
	home := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if err := codexCredentialPreflight(nil, env(map[string]string{"HOME": home}), os.Stat); !errors.Is(err, ErrCodexCredentials) {
		t.Fatalf("no creds: %v", err)
	}
	if err := codexCredentialPreflight(map[string]string{"OPENAI_API_KEY": "k"}, env(map[string]string{"HOME": home}), os.Stat); err != nil {
		t.Fatalf("api key: %v", err)
	}
	if err := codexCredentialPreflight(nil, env(map[string]string{"HOME": home, SkipCodexPreflightEnv: "1"}), os.Stat); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexCredentialPreflight(nil, env(map[string]string{"HOME": home}), os.Stat); err != nil {
		t.Fatalf("auth.json: %v", err)
	}
}

func TestExitDetailRedactsAndBounds(t *testing.T) {
	got := exitDetail("Error: bad key sk-abcdefgh12345 and SECRETVALUE1", []string{"SECRETVALUE1"})
	if got != "last stderr: Error: bad key [redacted] and [REDACTED]" && !strings.Contains(got, "[redacted]") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "sk-abcdefgh") || strings.Contains(got, "SECRETVALUE1") {
		t.Fatalf("leaked: %q", got)
	}
	if lastLine("par", []byte("tial\nsecond\n")) != "second" {
		t.Fatal("lastLine")
	}
}
