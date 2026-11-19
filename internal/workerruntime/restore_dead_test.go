package workerruntime_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/shim"

	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// hostDir writes a session host dir the way the executor leaves one: a private meta.json naming a
// private launch descriptor whose control dir holds control.sock. socket says what is at the socket
// path: "live" (a fake shim that completes the controller handshake), "refused" (a socket file with
// no listener) or "" (none).
func hostDir(t *testing.T, root, name, socket string, output int) {
	t.Helper()
	dir := filepath.Join(root, "hosts", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	control, err := os.MkdirTemp("", "cs") // short: unix socket paths are limited to 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(control) })
	launch := filepath.Join(dir, "launch.json")
	spec, _ := json.Marshal(shim.Launch{Session: name, Instance: "i1", ControlDir: control, Secret: "0123456789abcdef0123456789abcdef"})
	if err := os.WriteFile(launch, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"id": name, "descriptor": launch, "state": "running"})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if output > 0 {
		f, err := os.Create(filepath.Join(dir, "output.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= output; i++ {
			_, _ = fmt.Fprintf(f, `{"frame":{"t":"output","seq":%d,"data":"line"},"cursor":"c%d"}`+"\n", i, i)
		}
		_ = f.Close()
	}
	sock := filepath.Join(control, "control.sock")
	switch socket {
	case "refused":
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		l.SetUnlinkOnClose(false)
		_ = l.Close()
	case "live":
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go fakeShim(l, name)
	}
}

func fakeShim(l net.Listener, session string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			frame := func(body string) shim.Frame {
				return shim.Frame{Major: shim.ProtocolMajor, Minor: shim.ProtocolMinor, Type: "hello", Session: session, Body: json.RawMessage(body)}
			}
			if shim.WriteFrame(c, frame(`{"nonce":"n1","controller_epoch":"e1"}`)) != nil {
				return
			}
			if _, err := shim.ReadFrame(c); err != nil {
				return
			}
			if shim.WriteFrame(c, frame(`{"controller_epoch":"e1","journal":"j1"}`)) != nil {
				return
			}
			for {
				if _, err := shim.ReadFrame(c); err != nil {
					return
				}
			}
		}()
	}
}

func age(t *testing.T, root, name string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	dir := filepath.Join(root, "hosts", name)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.Chtimes(filepath.Join(dir, e.Name()), old, old)
	}
	_ = os.Chtimes(dir, old, old)
}

// CW-20261114-0056: agent-os had 344 host dirs, 309 dead, and restore outran outpost's plugin init
// deadline. 300 dead dirs (no session record, no descriptor, a refused or missing socket; some with
// long output) and 3 live ones restore in well under 2s, the live ones re-adopted; dead dirs past the
// grace age are moved to hosts-dead/<date>/, younger ones stay; the counts are kept for the hello.
func TestRestoreSkipsDeadHostsCheaplyAndPrunesOldOnes(t *testing.T) {
	root := t.TempDir()
	for i := range 3 {
		hostDir(t, root, fmt.Sprintf("live-%d", i), "live", 50)
	}
	old := 0
	for i := range 300 {
		name := fmt.Sprintf("dead-%03d", i)
		switch i % 4 {
		case 0: // the shim exited and its socket file was left behind
			hostDir(t, root, name, "refused", 2000)
		case 1: // the socket is gone too
			hostDir(t, root, name, "", 0)
		case 2: // no launch descriptor
			hostDir(t, root, name, "", 0)
			_ = os.Remove(filepath.Join(root, "hosts", name, "launch.json"))
		case 3: // no session record at all
			if err := os.MkdirAll(filepath.Join(root, "hosts", name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if i%2 == 0 {
			age(t, root, name, workerruntime.DeadHostGrace+time.Hour)
			old++
		} else {
			age(t, root, name, workerruntime.DeadHostGrace-time.Hour)
		}
	}
	age(t, root, "live-0", 3*workerruntime.DeadHostGrace) // an old live host is never pruned

	e := workerruntime.NewExecutor(root, "unused", func(workerwire.Frame) error { return nil })
	start := time.Now()
	if err := e.Restore(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("restore took %s with 300 dead hosts", took)
	}
	if got := len(e.Inventory()); got != 3 {
		t.Fatalf("re-adopted %d sessions, want 3", got)
	}
	want := workerwire.RestoreCounts{Live: 3, DeadSkipped: 300, Pruned: old}
	if got := e.RestoreCounts(); got == nil || *got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	left, _ := os.ReadDir(filepath.Join(root, "hosts"))
	if len(left) != 3+300-old {
		t.Fatalf("%d dirs left in hosts, want %d", len(left), 3+300-old)
	}
	moved, _ := os.ReadDir(filepath.Join(root, "hosts-dead", time.Now().UTC().Format("20060102")))
	if len(moved) != old {
		t.Fatalf("%d dirs in hosts-dead, want %d", len(moved), old)
	}
	for _, d := range left {
		if _, err := os.Stat(filepath.Join(root, "hosts-dead", time.Now().UTC().Format("20060102"), d.Name())); err == nil {
			t.Fatalf("%s is in both places", d.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "hosts", "live-0")); err != nil {
		t.Fatalf("an old live host was moved: %v", err)
	}
}
