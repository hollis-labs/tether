package workerruntime

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// A stop kills the agent but the shim-host keeps serving until signaled. When the agent's exit arrives the
// worker must end the host's process group and remove its control socket, or every stopped session leaks one.
func TestAgentExitTearsDownShimHost(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"c", "j"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"c/control.sock", "pin", "launch.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := exec.Command("sleep", "300")
	host.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = host.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = host.Process.Kill() })

	frames := make(chan shim.Frame, 1)
	s := &execution{ID: "s", Descriptor: filepath.Join(dir, "launch.json"), State: "running", HostPID: host.Process.Pid,
		client: &shim.Client{Frames: frames}, pending: map[string]chan shim.Frame{}}
	e := &Executor{Root: dir, Send: func(workerwire.Frame) error { return nil }, sessions: map[string]*execution{"s": s}}
	done := make(chan struct{})
	go func() { e.follow(s); close(done) }()

	payload, _ := json.Marshal(shim.Exit{})
	body, _ := json.Marshal(map[string]any{"event": mesh.Event{Kind: "shim.exit", Payload: payload}})
	frames <- shim.Frame{Type: "event", Body: body}
	close(frames)
	<-done

	select {
	case <-reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("shim-host still running after the agent exited")
	}
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, "c")); os.IsNotExist(err) {
			break
		}
		if i > 200 {
			t.Fatal("shim control dir not cleaned up")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
