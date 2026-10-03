//go:build linux

package shimhost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
)

func placedHost(t *testing.T, alter func(*Config, *shim.Launch)) (*Provider, Receipt) {
	t.Helper()
	cfg, spec := hostSpec(t)
	if alter != nil {
		alter(&cfg, &spec)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r, err := p.Place(ctx, "recovery-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx, r)
		for _, pid := range []int{r.ProviderPID, r.HostPID} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	t.Logf("host=%d provider=%d", r.HostPID, r.ProviderPID)
	return p, r
}
func TestStopAfterNaturalExitRemovesCapabilityAndIsIdempotent(t *testing.T) {
	p, r := placedHost(t, func(_ *Config, spec *shim.Launch) { spec.Env[0] = "TETHER_TEST_SHIM=child-exit" })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		inspection, err := p.Inspect(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if !inspection.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.Stop(ctx, r); err != nil {
		t.Fatalf("natural exit teardown: %v", err)
	}
	if !errors.Is(syscall.Kill(r.HostPID, 0), syscall.ESRCH) {
		t.Fatal("host survived stop")
	}
	if _, err := os.Stat(r.DescriptorPath); !os.IsNotExist(err) {
		t.Fatal("secret descriptor retained")
	}
	if _, err := os.Stat(metadataPath(r)); err != nil {
		t.Fatal("secret-free receipt removed")
	}
	if err := p.Stop(ctx, r); err != nil {
		t.Fatalf("repeat stop: %v", err)
	}
}
func TestStopEscalatesIgnoringHostWithBackgroundContext(t *testing.T) {
	p, r := placedHost(t, func(cfg *Config, _ *shim.Launch) { cfg.HostEnv = append(cfg.HostEnv, "TETHER_TEST_IGNORE_TERM=1") })
	done := make(chan error, 1)
	go func() { done <- p.Stop(context.Background(), r) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		_ = syscall.Kill(r.HostPID, syscall.SIGKILL)
		<-done
		t.Fatal("Stop has no bounded escalation")
	}
	if !errors.Is(syscall.Kill(r.HostPID, 0), syscall.ESRCH) {
		t.Fatal("host survived escalation")
	}
}
func TestInspectPreservesPlacementIntent(t *testing.T) {
	p, r := placedHost(t, nil)
	reconstructed := r
	reconstructed.OperationKey = ""
	reconstructed.Fingerprint = ""
	reconstructed.Attempted = false
	if _, err := p.Inspect(context.Background(), reconstructed); err != nil {
		t.Fatal(err)
	}
	var saved Receipt
	if err := ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.OperationKey != r.OperationKey || saved.Fingerprint != r.Fingerprint || !saved.Attempted {
		t.Fatal("Inspect replaced durable placement intent")
	}
}
func TestPlacementRetryIgnoresDeclaredVolatileEnvValue(t *testing.T) {
	cfg, spec := hostSpec(t)
	// Supports reproducing the regression with the earlier Config.
	names := reflect.ValueOf(&cfg).Elem().FieldByName("VolatileEnvKeys")
	if names.IsValid() {
		names.Set(reflect.ValueOf([]string{"SESSION_TOKEN"}))
	}
	spec.Env = append(spec.Env, "SESSION_TOKEN=first")
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Place(context.Background(), "retry-key", spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.Stop(ctx, r)
		_ = syscall.Kill(r.HostPID, syscall.SIGKILL)
	})
	spec.Env[len(spec.Env)-1] = "SESSION_TOKEN=re-minted"
	for i, j := 0, len(spec.Env)-1; i < j; i, j = i+1, j-1 {
		spec.Env[i], spec.Env[j] = spec.Env[j], spec.Env[i]
	}
	again, err := p.Place(context.Background(), "retry-key", spec)
	if err != nil || again.HostPID != r.HostPID {
		t.Fatalf("volatile retry: %+v %v", again, err)
	}
}
func TestPlaceWaitsForLockAndReturnsTypedBusy(t *testing.T) {
	cfg, spec := hostSpec(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.dir(spec.Session), "placement.lock")
	lock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	_, err = p.Place(ctx, "key", spec)
	cancel()
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "busy" {
		t.Errorf("lock contention: %v", err)
	}
	released := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); _ = lock.Close(); close(released) }()
	r, err := p.Place(context.Background(), "key", spec)
	<-released
	if err != nil {
		t.Fatalf("placement did not wait: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.Stop(ctx, r)
		_ = syscall.Kill(r.HostPID, syscall.SIGKILL)
	})
}
func TestPlacementClearsStaleCommitFiles(t *testing.T) {
	cfg, spec := hostSpec(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := p.dir(spec.Session)
	if err = PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".commit-abandoned")
	if err = os.WriteFile(stale, []byte("discarded capability"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := p.Place(context.Background(), "key", spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.Stop(ctx, r)
		_ = syscall.Kill(r.HostPID, syscall.SIGKILL)
	})
	if _, err = os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale secret temp file retained")
	}
}
func TestHostJournalPinRefusesBeforeEpochChanges(t *testing.T) {
	p, r := placedHost(t, nil)
	before, err := p.Inspect(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	wrong := r
	wrong.Journal = "foreign"
	if err = p.Stop(context.Background(), wrong); err == nil {
		t.Fatal("wrong journal accepted")
	}
	after, err := p.Inspect(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if after.Receipt.Epoch != before.Receipt.Epoch {
		t.Fatal("wrong journal changed controller epoch")
	}
	if _, err = p.Reattach(context.Background(), wrong); err == nil {
		t.Fatal("reattach wrong journal accepted")
	}
}
func TestLongTempDirectoryDoesNotChangeSocketPlacement(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("long", 25))
	if err := os.Mkdir(long, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	p, r := placedHost(t, nil)
	if err := p.Stop(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// A peer that removes its socket after authenticated hello cannot make a stored
// PID into a signaling authority. The unrelated child must stay alive.
func TestStopRefusesStoredPIDWhenAuthenticatedPeerDiffers(t *testing.T) {
	cfg, spec := hostSpec(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec.ControlDir = filepath.Join(spec.Cwd, "c")
	spec.Secret = strings.Repeat("s", 32)
	if err = PrivateDir(spec.ControlDir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spec.Cwd, "launch.json")
	if err = WritePrivateJSON(path, spec); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(spec.ControlDir, "control.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	victim := exec.Command(exe, "-test.run=^TestHostProcess$")
	victim.Env = []string{"TETHER_TEST_SHIM=child", "HOME=" + spec.Cwd, "TMPDIR=" + spec.Cwd}
	in, err := victim.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("victim pid=%d", victim.Process.Pid)
	waited := make(chan error, 1)
	go func() { waited <- victim.Wait() }()
	defer func() { _ = in.Close(); _ = victim.Process.Kill(); <-waited }()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, e := listener.AcceptUnix()
		if e != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		body, _ := json.Marshal(map[string]string{"nonce": "nonce", "controller_epoch": "0"})
		_ = shim.WriteFrame(conn, shim.Frame{Major: 1, Type: "hello", Session: spec.Session, Body: body})
		if _, e = shim.ReadFrame(conn); e != nil {
			return
		}
		_ = os.Remove(socket)
		body, _ = json.Marshal(map[string]string{"controller_epoch": "1", "journal": "journal"})
		_ = shim.WriteFrame(conn, shim.Frame{Major: 1, Type: "hello", Session: spec.Session, Body: body})
		for {
			frame, e := shim.ReadFrame(conn)
			if e != nil {
				return
			}
			body = json.RawMessage(`{}`)
			if frame.Type == "health" {
				body = json.RawMessage(`{"running":false,"pid":0}`)
			}
			_ = shim.WriteFrame(conn, shim.Frame{Major: 1, Type: "result", Session: spec.Session, ReplyTo: frame.RequestID, Body: body})
		}
	}()
	r := Receipt{Session: spec.Session, Instance: spec.Instance, Generation: spec.Generation, DescriptorPath: path, SocketPath: socket, Backend: Detached, HostPID: victim.Process.Pid, Journal: "journal"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = p.Stop(ctx, r)
	<-serverDone
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "identity_mismatch" {
		t.Errorf("stale identity: %v", err)
	}
	if !errors.Is(syscall.Kill(victim.Process.Pid, 0), nil) {
		t.Fatal("unrelated stored PID was signaled")
	}
}

func TestHostPIDMismatchRefusesBeforeEpochChanges(t *testing.T) {
	p, r := placedHost(t, nil)
	before, err := p.Inspect(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	wrong := r
	wrong.HostPID = os.Getpid()
	err = p.Stop(context.Background(), wrong)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "identity_mismatch" {
		t.Fatalf("PID mismatch: %v", err)
	}
	after, err := p.Inspect(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if after.Receipt.Epoch != before.Receipt.Epoch {
		t.Fatal("PID refusal changed controller epoch")
	}
}
