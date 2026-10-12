package outpostmail

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This test binary is also the fake Codex executable: asserts argv, no shell.
func TestAdapterProcess(t *testing.T) {
	if os.Getenv("OUTPOST_ADAPTER_HELPER") == "1" {
		if len(os.Args) > 1 && os.Args[1] == "queue" {
			if path := os.Getenv("OUTPOST_ADAPTER_CAPTURE"); path != "" {
				b, _ := json.Marshal(os.Args[1:])
				_ = os.WriteFile(path, b, 0600)
			}
			_ = json.NewEncoder(os.Stdout).Encode(os.Args[1:])
			os.Exit(0)
		}
		os.Exit(2)
	}
}
func TestOutpostMailS3(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Execute(ctx, "codex-queue", "-option", "text"); err == nil {
		t.Fatal("option-like handle accepted")
	}
	if err := Execute(ctx, "tmux-paste", "0", "text"); err == nil {
		t.Fatal("non-pane target accepted")
	}
	if err := Execute(ctx, "shell", "rm", "text"); err == nil {
		t.Fatal("unknown adapter accepted")
	}
	bin := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(exe, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(bin, "argv.json")
	t.Setenv("OUTPOST_ADAPTER_HELPER", "1")
	t.Setenv("OUTPOST_ADAPTER_CAPTURE", capture)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	literal := "mail $(must-not-execute); --options are text"
	if err = Execute(ctx, "codex-queue", "thread", literal); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err = json.Unmarshal(b, &args); err != nil {
		t.Fatal(err)
	}
	if len(args) != 5 || args[0] != "queue" || args[1] != "--thread" || args[2] != "thread" || args[3] != "--message" || args[4] != literal {
		t.Fatalf("argv %v", args)
	}
	if os.Getenv("OUTPOST_NATIVE_SMOKE") != "1" {
		t.Skip("scripts/outpost-mail-smoke-s3.sh enables isolated native tmux smoke")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "outpost-smoke-" + filepath.Base(dir)
	run := func(args ...string) []byte {
		t.Helper()
		b, err := exec.Command("tmux", append([]string{"-L", name}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatal(string(b), err)
		}
		return b
	}
	file := filepath.Join(dir, "mail.txt")
	// The only shell here is the owned scratch fixture, never delivery text.
	info := strings.TrimSpace(string(run("new-session", "-d", "-s", "probe", "-P", "-F", "#{socket_path},#{pid},#{pane_id}", "/bin/sh -c 'read -r line; printf \"%s\" \"$line\" > \""+file+"\"; sleep 2'")))
	defer func() { _ = exec.Command("tmux", "-L", name, "kill-server").Run() }()
	parts := strings.Split(info, ",")
	if len(parts) != 3 {
		t.Fatal(info)
	}
	t.Setenv("TMUX", info)
	text := "mail $(must-not-execute) ; literal"
	if err := Execute(ctx, "tmux-paste", parts[2], text); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		b, err := os.ReadFile(file)
		if err == nil {
			if string(b) != text {
				t.Fatalf("paste %q", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
