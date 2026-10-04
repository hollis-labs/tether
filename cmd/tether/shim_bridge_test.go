//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/testutil"
)

func TestShimBridgeCLIChild(t *testing.T) {
	if os.Getenv("TETHER_BRIDGE_CLI_CHILD") == "bridge-error" {
		os.Args = []string{os.Args[0], "shim-bridge", "--descriptor", "/missing/launch.json"}
		main()
		os.Exit(0)
	}
	if os.Getenv("TETHER_BRIDGE_CLI_CHILD") == "" {
		return
	}
	_, _ = fmt.Fprint(os.Stdout, `{"type":"system","subtype":"init","session_id":"cli-test"}`+"\n")
	os.Exit(7)
}
func TestShimBridgeCommandExitAndExplicitDescriptor(t *testing.T) {
	cmd := shimBridgeCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("implicit descriptor accepted")
	}
	dir := testutil.ShortDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "pin"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	spec := shim.Launch{Session: "urn:session:cli", Instance: "urn:instance:cli", Generation: 1, Actor: mesh.Actor{URN: "msg://service/shim/test", Kind: mesh.ActorService}, Subject: "urn:session:cli", Argv: []string{exe, "-test.run=^TestShimBridgeCLIChild$"}, Env: []string{"TETHER_BRIDGE_CLI_CHILD=1", "HOME=" + dir, "TMPDIR=" + dir}, Cwd: dir, ControlDir: filepath.Join(dir, "c"), JournalDir: filepath.Join(dir, "j"), Secret: strings.Repeat("s", 32), PinPath: filepath.Join(dir, "pin"), PinKey: "cli", BootGeneration: "cli", Reservation: "cli", StopGrace: time.Millisecond}
	path := filepath.Join(dir, "launch.json")
	if err = shimhost.WritePrivateJSON(path, spec); err != nil {
		t.Fatal(err)
	}
	host, err := shim.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close() }()
	client, err := shim.Connect(host.SocketPath(), spec.Secret, spec.Session, spec.Instance, "1", "observer", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Send(spec.Session, "health", map[string]string{"ping": "pid"}); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	providerPID := 0
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for providerPID == 0 {
		select {
		case frame := <-client.Frames:
			if frame.Type == "result" {
				var health struct {
					PID int `json:"pid"`
				}
				if err = json.Unmarshal(frame.Body, &health); err != nil {
					t.Fatal(err)
				}
				providerPID = health.PID
			}
		case <-deadline.C:
			_ = client.Close()
			t.Fatal("provider pid unavailable")
		}
	}
	_ = client.Close()
	t.Logf("provider pid=%d", providerPID)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	output := &bytes.Buffer{}
	cmd = shimBridgeCmd()
	cmd.SetIn(reader)
	cmd.SetOut(output)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--descriptor", path})
	err = cmd.Execute()
	var exit *shimBridgeExit
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("exit not propagated: %v", err)
	}
	if !strings.Contains(output.String(), "cli-test") {
		t.Fatal("provider init missing")
	}
	child := host.Wait()
	t.Logf("provider terminal exit=%d signal=%d", child.Status, child.Signal)
	// The test owns no daemon or service. The shim's Close reaps its process.
	if !errors.Is(syscall.Kill(providerPID, 0), syscall.ESRCH) {
		t.Fatal("provider remains alive")
	}
	if child.Status != 7 || child.Signal != 0 {
		t.Fatal("fake child not exited")
	}
}

func TestShimBridgeInfrastructureFailureHasDistinctExitCode(t *testing.T) {
	cmd := shimBridgeCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--descriptor", "/missing/launch.json"})
	err := cmd.Execute()
	var classified exitCoder
	if !errors.As(err, &classified) || classified.ExitCode() != 93 {
		t.Fatalf("infrastructure exit: %v", err)
	}
}

func TestShimBridgeMainMapsInfrastructureFailure(t *testing.T) {
	dir := testutil.ShortDir(t)
	defer func() { _ = os.RemoveAll(dir) }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestShimBridgeCLIChild$")
	cmd.Env = []string{"TETHER_BRIDGE_CLI_CHILD=bridge-error", "OTEL_SDK_DISABLED=true", "HOME=" + dir, "TMPDIR=" + dir}
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 93 {
		t.Fatalf("main mapped failure to %v: %s", err, output)
	}
	t.Logf("bridge pid=%d exited=93", cmd.Process.Pid)
}
