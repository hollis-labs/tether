package workerruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/libs/harness/core/shim"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// lingeringTempDir is a temp dir whose removal tolerates the in-process shim host and the persist of the last
// session state still writing for a moment after the test body ends (t.TempDir fails the test on that race).
func lingeringTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "worker-resume-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Twice, with a pause: the follower's last persist can recreate the host dir just after the first removal.
		for pass := 0; pass < 2; pass++ {
			for i := 0; i < 40; i++ {
				if os.RemoveAll(dir) == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("could not remove %s: %v", dir, err)
		}
	})
	return dir
}

// fakeClaude installs a "claude" on PATH that records its cwd and argv to $RESUME_RECORD and exits.
func fakeClaude(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n{ pwd; for a in \"$@\"; do echo \"$a\"; done; } > \"$RESUME_RECORD\"\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: a test fixture that must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A fake hub drives a real worker over the websocket: the worker's hello must advertise worker-resume, a launch with
// resume_session_id must continue the provider session in the workspace it ran in (claude --resume <id>, the hub's
// stale resume flags dropped), and a resume whose provider session is gone must come back as launch_failed with the
// typed resume_unavailable error and no fresh start.
func TestWorkerResumeRoundTripWithFakeHub(t *testing.T) {
	fakeClaude(t)
	cfgDir := t.TempDir()
	root := lingeringTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "hub-mcp.json"), []byte(testHubMCP), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWork := filepath.Join(root, "workspaces", "old-session", "workroot")
	if err := os.MkdirAll(oldWork, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript(t, cfgDir, "native-abc-1", oldWork)
	record := filepath.Join(t.TempDir(), "record")

	hello := make(chan workerwire.Frame, 1)
	replies := make(chan workerwire.Frame, 8)
	launch := func(session, resumeID string) workerwire.Frame {
		spec := workerwire.LaunchSpec{Session: session, Actor: "msg://agent/agent-mux/agt_resume_probe", Provider: "claude", PermissionMode: "bypass", RuntimeKind: "streaming-stdio",
			Args: []string{"--model", "m", "--resume", "stale-id"}, Env: map[string]string{"RESUME_RECORD": record}, MCPServers: []string{}, ResumeSessionID: resumeID}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		return workerwire.Frame{T: "launch", CmdID: "cmd-" + session, Session: session, Spec: raw}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		var f workerwire.Frame
		if ws.ReadJSON(&f) != nil {
			return
		}
		hello <- f
		_ = ws.WriteJSON(workerwire.Frame{T: "welcome", Features: []string{"bind", "deliver"}})
		_ = ws.WriteJSON(launch("missing-session", "no-such-native-1"))
		_ = ws.WriteJSON(launch("resumed-session", "native-abc-1"))
		for ws.ReadJSON(&f) == nil {
			if f.T == "launched" || f.T == "launch_failed" {
				replies <- f
			}
		}
	}))
	defer server.Close()

	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "device.token"), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{Root: root, Config: Config{Hub: server.URL, Label: "resume-probe"}, Caps: func(context.Context) json.RawMessage { return json.RawMessage(`{}`) }}
	e := NewExecutor(root, "unused", w.Send)
	e.ClaudeDir = cfgDir
	// Run the shim host in-process instead of re-executing the worker binary.
	var hosts []*shim.Host
	e.Spawn = func(descriptor string) (*exec.Cmd, error) {
		spec, err := shim.ReadLaunch(descriptor)
		if err != nil {
			return nil, err
		}
		h, err := shim.Start(spec)
		if err == nil {
			hosts = append(hosts, h)
		}
		return nil, err
	}
	w.Command = e.Command
	w.Inventory = e.Inventory
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = w.Run(ctx) }()
	// Registered after the TempDirs, so it runs before they are removed: nothing may still be writing into root.
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
		}
		for _, h := range hosts {
			_ = h.Close()
		}
	})

	select {
	case f := <-hello:
		advertised := false
		for _, feat := range f.Features {
			advertised = advertised || feat == workerwire.FeatureWorkerResume
		}
		if !advertised {
			t.Fatalf("hello features %v lack %q", f.Features, workerwire.FeatureWorkerResume)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no hello")
	}

	got := map[string]workerwire.Frame{}
	for len(got) < 2 {
		select {
		case f := <-replies:
			got[f.Session] = f
		case <-time.After(10 * time.Second):
			t.Fatalf("replies so far: %+v", got)
		}
	}
	failed := got["missing-session"]
	if failed.T != "launch_failed" || !strings.HasPrefix(failed.Error, "resume_unavailable:") {
		t.Fatalf("missing provider session: %+v, want launch_failed resume_unavailable", failed)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", "missing-session")); err == nil {
		t.Fatal("a refused resume must not leave a workspace behind")
	}
	ok := got["resumed-session"]
	if ok.T != "launched" || ok.Workspace != oldWork {
		t.Fatalf("resumed launch: %+v, want launched in %s", ok, oldWork)
	}

	var rec []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(record)
		if rec = strings.Split(strings.TrimSpace(string(b)), "\n"); len(rec) > 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake claude never recorded argv: %q", rec)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rec[0] != oldWork {
		t.Fatalf("claude ran in %q, want the original workspace %q", rec[0], oldWork)
	}
	args := strings.Join(rec[1:], " ")
	if strings.Contains(args, "stale-id") || strings.Count(args, "--resume") != 1 || !strings.Contains(args, "--resume native-abc-1") {
		t.Fatalf("claude args = %q, want exactly --resume native-abc-1", args)
	}
}

// A resume for a provider the worker cannot natively resume is refused with the typed error.
func TestLaunchRefusesResumeForNonClaudeProvider(t *testing.T) {
	e := NewExecutor(t.TempDir(), "unused", func(workerwire.Frame) error { return nil })
	raw, _ := json.Marshal(workerwire.LaunchSpec{Session: "s1", Provider: "sh", ResumeSessionID: "native-1"})
	err := e.Command(context.Background(), workerwire.Frame{T: "launch", Session: "s1", Spec: raw})
	if err == nil || !strings.HasPrefix(err.Error(), "resume_unavailable:") {
		t.Fatalf("err = %v, want resume_unavailable", err)
	}
}
