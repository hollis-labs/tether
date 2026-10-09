package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyNativeStateHistoryOnly(t *testing.T) {
	for brand, stateDir := range map[string]string{"codex": "sessions", "claude": "projects"} {
		t.Run(brand, func(t *testing.T) {
			source, target := t.TempDir(), t.TempDir()
			state := filepath.Join(stateDir, "owned-project", "turn.jsonl")
			writeNativeFixture(t, source, state, "native conversation")
			writeNativeFixture(t, source, "auth.json", "old fixture auth")
			writeNativeFixture(t, source, "config.toml", "old fixture config")
			writeNativeFixture(t, target, "auth.json", "new fixture auth")
			writeNativeFixture(t, target, "config.toml", "new fixture config")
			if err := CopyNativeState(source, target, brand); err != nil {
				t.Fatal(err)
			}
			assertNativeFixture(t, target, state, "native conversation")
			assertNativeFixture(t, source, state, "native conversation")
			assertNativeFixture(t, target, "auth.json", "new fixture auth")
			assertNativeFixture(t, target, "config.toml", "new fixture config")
			info, err := os.Stat(filepath.Join(target, state))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("copied state mode: info=%v err=%v", info, err)
			}
			if err := CopyNativeState(source, target, brand); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing destination must be refused: %v", err)
			}
			assertNativeFixture(t, target, state, "native conversation")
		})
	}
}

func TestCopyNativeStateSameHomePreservesHistory(t *testing.T) {
	home := t.TempDir()
	writeNativeFixture(t, home, "sessions/turn.jsonl", "same-id history")
	if err := CopyNativeState(home, filepath.Join(home, "."), "codex"); err != nil {
		t.Fatal(err)
	}
	assertNativeFixture(t, home, "sessions/turn.jsonl", "same-id history")
}

func TestCopyNativeStateRejectsUnsafeInputs(t *testing.T) {
	for _, kind := range []string{"home-link", "state-link", "file-link", "directory-link", "target-link", "missing-home", "missing-state", "unsupported-brand", "relative-home"} {
		t.Run(kind, func(t *testing.T) {
			source, target, outside := t.TempDir(), t.TempDir(), t.TempDir()
			writeNativeFixture(t, outside, "auth.json", "outside fixture")
			writeNativeFixture(t, source, "sessions/turn.jsonl", "owned history")
			brand := "codex"
			switch kind {
			case "home-link":
				link := filepath.Join(t.TempDir(), "home")
				mustNativeSymlink(t, source, link)
				source = link
			case "state-link":
				source = t.TempDir()
				mustNativeSymlink(t, outside, filepath.Join(source, "sessions"))
			case "file-link":
				mustNativeSymlink(t, filepath.Join(outside, "auth.json"), filepath.Join(source, "sessions", "linked.jsonl"))
			case "directory-link":
				mustNativeSymlink(t, outside, filepath.Join(source, "sessions", "linked"))
			case "target-link":
				link := filepath.Join(t.TempDir(), "target")
				mustNativeSymlink(t, target, link)
				target = link
			case "missing-state":
				source = t.TempDir()
			case "missing-home":
				source = filepath.Join(t.TempDir(), "absent-home")
			case "unsupported-brand":
				brand = "other"
			case "relative-home":
				source = "relative-home"
			}
			err := CopyNativeState(source, target, brand)
			if err == nil {
				t.Fatal("unsafe input accepted")
			}
			wantMissing := kind == "missing-home" || kind == "missing-state"
			if errors.Is(err, ErrNativeStateMissing) != wantMissing {
				t.Fatalf("missing state classification: err=%v wantMissing=%v", err, wantMissing)
			}
			if _, err := os.Lstat(filepath.Join(target, "sessions")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal created target state: %v", err)
			}
			assertNativeFixture(t, outside, "auth.json", "outside fixture")
		})
	}
}

func writeNativeFixture(t *testing.T, root, path, content string) {
	t.Helper()
	destination := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNativeFixture(t *testing.T, root, path, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, path))
	if err != nil || string(got) != want {
		t.Fatalf("fixture %s: got %q err=%v", path, got, err)
	}
}

func mustNativeSymlink(t *testing.T, source, target string) {
	t.Helper()
	if err := os.Symlink(source, target); err != nil {
		t.Fatal(err)
	}
}
