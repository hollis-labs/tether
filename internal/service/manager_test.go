package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceEchoFixture(t *testing.T) {
	if os.Getenv("TETHER_SERVICE_ECHO_FIXTURE") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Fprintln(os.Stdout, scanner.Text())
	}
	os.Exit(0)
}

type echoFixture struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Scanner
	closed  bool
}

func startEchoFixture(t *testing.T) *echoFixture {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceEchoFixture$")
	command.WaitDelay = 200 * time.Millisecond
	command.Env = []string{"HOME=" + dir, "TMPDIR=" + dir, "TETHER_SERVICE_ECHO_FIXTURE=1"}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &echoFixture{command: command, stdin: stdin, stdout: bufio.NewScanner(stdout)}
	t.Cleanup(func() { fixture.close(t) })
	return fixture
}
func (f *echoFixture) close(t *testing.T) {
	t.Helper()
	if f.closed {
		return
	}
	f.closed = true
	if err := f.stdin.Close(); err != nil {
		t.Error(err)
	}
	if err := f.command.Wait(); err != nil {
		t.Error(err)
	}
}
func (f *echoFixture) roundTrip(t *testing.T, message string) {
	t.Helper()
	if _, err := fmt.Fprintln(f.stdin, message); err != nil {
		t.Fatal(err)
	}
	if !f.stdout.Scan() || f.stdout.Text() != message {
		t.Fatal("disposable host did not answer", f.stdout.Err())
	}
}

func TestServiceRestartRetainsIndependentDisposableHost(t *testing.T) {
	f := newFakeManager(t)
	f.install(t, "0.8.0")
	host := startEchoFixture(t)
	daemon := startEchoFixture(t)
	host.roundTrip(t, "before")
	hostPID := host.command.Process.Pid
	daemonPID := daemon.command.Process.Pid
	f.manager.Run = func(ctx context.Context, command string, args ...string) (string, error) {
		output, err := f.run(ctx, command, args...)
		if err == nil && command == "systemctl" && len(args) > 2 && args[1] == "restart" && args[2] == UnitName {
			daemon.close(t)
			daemon = startEchoFixture(t)
		}
		return output, err
	}
	if err := f.manager.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if daemon.command.Process.Pid == daemonPID {
		t.Fatal("disposable daemon was not replaced")
	}
	if host.command.Process.Pid != hostPID {
		t.Fatal("host custody was replaced")
	}
	host.roundTrip(t, "after")
	daemon.roundTrip(t, "new-daemon")
	// This is a process/command-boundary model, not a real systemd/provider
	// survival receipt. Clean-account logout and live shim acceptance stay open.
}

func TestServiceRootAndUnknownLingerRefused(t *testing.T) {
	f := newFakeManager(t)
	f.manager.UID = "0"
	archive, checksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	if err := f.manager.Install(context.Background(), "0.8.0", archive, checksums); err == nil {
		t.Fatal("root install accepted")
	}
	f.manager.UID = "1001"
	f.linger = "unknown"
	if err := f.manager.Install(context.Background(), "0.8.0", archive, checksums); err == nil {
		t.Fatal("unknown linger accepted")
	}
	if len(f.mutations) > 0 {
		t.Fatal("prerequisite refusal mutated service")
	}
}

type fakeManager struct {
	manager     *Manager
	linger      string
	active      string
	enabled     string
	fragment    string
	failRestart bool
	mutations   []string
}

func newFakeManager(t *testing.T) *fakeManager {
	t.Helper()
	r := testRuntime(t)
	f := &fakeManager{linger: "yes", active: "inactive", enabled: "disabled"}
	f.manager = &Manager{Runtime: r, UnitDir: filepath.Join(filepath.Dir(r.Root), "units"), Catalog: filepath.Join(filepath.Dir(r.Root), "catalog"), PathEnv: "/custom/providers:/usr/bin", UID: "1001", User: "worker"}
	f.manager.Run = f.run
	f.manager.DaemonPID = func() (int, error) {
		if f.active == "active" {
			return 123, nil
		}
		return 0, nil
	}
	return f
}
func (f *fakeManager) run(ctx context.Context, command string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if command == "loginctl" {
		return f.linger + "\n", nil
	}
	if command != "systemctl" || len(args) < 2 || args[0] != "--user" {
		return "", fmt.Errorf("unexpected service command")
	}
	if args[1] == "show" {
		fragment := f.fragment
		if fragment == "" {
			if _, err := os.Lstat(f.manager.unitPath()); err == nil {
				fragment = f.manager.unitPath()
			}
		}
		if fragment == "" {
			return "LoadState=not-found\n", nil
		}
		return "LoadState=loaded\nFragmentPath=" + fragment + "\nActiveState=" + f.active + "\nUnitFileState=" + f.enabled + "\nExecMainPID=123\n", nil
	}
	f.mutations = append(f.mutations, strings.Join(args, " "))
	switch args[1] {
	case "daemon-reload":
		return "", nil
	case "enable":
		f.enabled = "enabled"
		return "", nil
	case "restart":
		if f.failRestart {
			f.active = "failed"
			return "", fmt.Errorf("synthetic start failure")
		}
		if _, err := f.manager.Runtime.Selector("current"); err != nil {
			return "", err
		}
		f.active = "active"
		return "", nil
	case "disable":
		f.enabled = "disabled"
		f.active = "inactive"
		return "", nil
	default:
		return "", fmt.Errorf("refusing any unrelated command")
	}
}
func (f *fakeManager) install(t *testing.T, version string) {
	t.Helper()
	archive, checksums := fixtureArchive(t, version, archiveMember{name: "tether", body: "synthetic " + version})
	if err := f.manager.Install(context.Background(), version, archive, checksums); err != nil {
		t.Fatal(err)
	}
}

func TestManagedServiceLifecycleAndManualSwitchBack(t *testing.T) {
	f := newFakeManager(t)
	f.install(t, "0.8.0")
	status := f.manager.Status(context.Background())
	if status.Ownership != "managed" || status.Current != "0.8.0" || len(status.Problems) != 0 {
		t.Fatalf("status: %+v", status)
	}
	archive, checksums := fixtureArchive(t, "0.8.1", archiveMember{name: "tether", body: "second"})
	if err := f.manager.Update(context.Background(), "0.8.1", archive, checksums); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.SwitchBack(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if version, _ := f.manager.Runtime.Selector("current"); version != "0.8.0" {
		t.Fatal("switch-back failed")
	}
	if err := f.manager.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Runtime.Ready("0.8.1"); err != nil {
		t.Fatal("uninstall deleted retained runtime", err)
	}
	if _, err := os.Lstat(f.manager.unitPath()); !os.IsNotExist(err) {
		t.Fatal("managed unit not removed", err)
	}
	for _, cmd := range f.mutations {
		if strings.Contains(cmd, "tether.service") || strings.Contains(cmd, "shim") || strings.Contains(cmd, "sudo") {
			t.Fatal("touched another supervisor or shim", cmd)
		}
	}
}

func TestServiceLingerFailureHasNoPublicationOrAccountChange(t *testing.T) {
	f := newFakeManager(t)
	f.linger = "no"
	archive, checksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	err := f.manager.Install(context.Background(), "0.8.0", archive, checksums)
	var problem *Problem
	if !errors.As(err, &problem) || problem.Code != "linger-disabled" || problem.Hint != "sudo loginctl enable-linger 'worker'" {
		t.Fatalf("linger diagnosis: %v", err)
	}
	if len(f.mutations) > 0 {
		t.Fatal("disabled linger caused service/account mutation")
	}
	if _, err := os.Lstat(f.manager.Runtime.Root); !os.IsNotExist(err) {
		t.Fatal("linger refusal published runtime", err)
	}
}

func TestExternalServiceNeverControlled(t *testing.T) {
	for _, kind := range []string{"marker", "foreign-unit", "foreign-fragment", "live-daemon", "tampered-unit"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeManager(t)
			if kind == "tampered-unit" {
				f.install(t, "0.8.0")
				if err := os.WriteFile(f.manager.unitPath(), []byte("external replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.manager.Runtime.prepare(); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "marker":
					if err := writeAtomic(f.manager.ownershipPath(), []byte("external\n")); err != nil {
						t.Fatal(err)
					}
				case "foreign-unit":
					if err := ensureDirectory(f.manager.UnitDir); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.manager.unitPath(), []byte("external unit"), 0600); err != nil {
						t.Fatal(err)
					}
				case "foreign-fragment":
					f.fragment = "/etc/systemd/user/" + UnitName
				case "live-daemon":
					f.manager.DaemonPID = func() (int, error) { return 999, nil }
				}
			}
			f.mutations = nil
			archive, checksums := fixtureArchive(t, "0.8.1", archiveMember{name: "tether", body: "fixture"})
			for _, op := range []func() error{
				func() error { return f.manager.Install(context.Background(), "0.8.1", archive, checksums) },
				func() error { return f.manager.Restart(context.Background()) },
				func() error { return f.manager.Update(context.Background(), "0.8.1", archive, checksums) },
				func() error { return f.manager.SwitchBack(context.Background(), "0.8.0") },
				func() error { return f.manager.Uninstall(context.Background()) },
			} {
				if err := op(); err == nil {
					t.Fatal("external ownership accepted")
				}
			}
			if len(f.mutations) > 0 {
				t.Fatal("external service controlled", f.mutations)
			}
		})
	}
}

func TestServiceFailedUpdateRetainsManualRecovery(t *testing.T) {
	f := newFakeManager(t)
	f.install(t, "0.8.0")
	archive, checksums := fixtureArchive(t, "0.8.1", archiveMember{name: "tether", body: "second"})
	f.failRestart = true
	if err := f.manager.Update(context.Background(), "0.8.1", archive, checksums); err == nil {
		t.Fatal("failed restart hidden")
	}
	if previous, _ := f.manager.Runtime.Selector("previous"); previous != "0.8.0" {
		t.Fatal("lost manual recovery version")
	}
	status := f.manager.Status(context.Background())
	found := false
	for _, p := range status.Problems {
		found = found || p.Code == "service-failed"
	}
	if !found {
		t.Fatalf("failure unexplained: %+v", status)
	}
	f.failRestart = false
	if err := f.manager.SwitchBack(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRefusesUnitInjectionAndUnknownDaemon(t *testing.T) {
	f := newFakeManager(t)
	f.manager.PathEnv = "/usr/bin\nExecStart=/evil"
	archive, checksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	if err := f.manager.Install(context.Background(), "0.8.0", archive, checksums); err == nil {
		t.Fatal("unit injection accepted")
	}
	if len(f.mutations) > 0 {
		t.Fatal("invalid unit caused mutation")
	}
	f.manager.PathEnv = "/usr/bin"
	f.manager.DaemonPID = func() (int, error) { return 0, fmt.Errorf("unknown") }
	if err := f.manager.Install(context.Background(), "0.8.0", archive, checksums); err == nil {
		t.Fatal("unknown daemon identity accepted")
	}
}

func TestServiceOutputBound(t *testing.T) {
	output := &boundedOutput{}
	_, _ = output.Write([]byte(strings.Repeat("x", 128<<10)))
	if !output.overflow || output.buffer.Len() > 64<<10 {
		t.Fatal("service command output exceeded bound")
	}
}

func TestManagedActiveUnitRequiresVerifiedDaemon(t *testing.T) {
	f := newFakeManager(t)
	f.install(t, "0.8.0")
	f.mutations = nil
	f.manager.DaemonPID = func() (int, error) { return 0, nil }
	if err := f.manager.Restart(context.Background()); err == nil {
		t.Fatal("active unit without verified daemon identity controlled")
	}
	if len(f.mutations) > 0 {
		t.Fatal("unknown active daemon restarted")
	}
	f.manager.DaemonPID = nil
	if err := f.manager.Uninstall(context.Background()); err == nil {
		t.Fatal("unverified active unit uninstalled")
	}
}

func TestServiceReadinessCancellation(t *testing.T) {
	f := newFakeManager(t)
	f.install(t, "0.8.0")
	f.manager.DaemonPID = func() (int, error) { return 0, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := f.manager.awaitRunning(ctx); err == nil {
		t.Fatal("unknown readiness reported success")
	}
}
