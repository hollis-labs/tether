//go:build linux

package shimhost

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
)

func testHostHandle(t *testing.T, r Receipt) *processHandle {
	t.Helper()
	spec, err := Descriptor(r)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Connect(context.Background(), r.SocketPath, spec.Secret, spec.Session, spec.Instance, strconv.FormatUint(spec.Generation, 10), "observer", r.Journal, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	handle, err := authenticatedProcess(c, r.HostPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handle.close)
	return handle
}
func assertRetiredStop(t *testing.T, p *Provider, r Receipt) {
	t.Helper()
	for range 2 {
		if err := p.Stop(context.Background(), r); err != nil {
			t.Fatalf("gone stop: %v", err)
		}
	}
	var saved Receipt
	if err := ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); err != nil || !saved.Retired {
		t.Fatalf("retirement: %+v %v", saved, err)
	}
	if _, err := os.Stat(r.DescriptorPath); !os.IsNotExist(err) {
		t.Fatal("launch capability retained")
	}
}
func TestStopRetiresCrashedHost(t *testing.T) {
	p, r := placedHost(t, func(_ *Config, s *shim.Launch) { s.Env[0] = "TETHER_TEST_SHIM=child-exit" })
	handle := testHostHandle(t, r)
	if err := handle.signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handle.wait(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	reaped := p.reaped[r.HostPID]
	p.mu.Unlock()
	select {
	case <-reaped:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	recovered := r
	recovered.HostPID = 0
	recovered.ShimPID = 0
	assertRetiredStop(t, p, recovered)
}
func TestStopRetiresAfterKillTimeout(t *testing.T) {
	p, r := placedHost(t, func(c *Config, _ *shim.Launch) {
		c.HostEnv = append(c.HostEnv, "TETHER_TEST_IGNORE_TERM=1")
		c.StopGrace = time.Second
		c.StopTimeout = 100 * time.Millisecond
	})
	handle := testHostHandle(t, r)
	if err := p.Stop(context.Background(), r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handle.wait(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	reaped := p.reaped[r.HostPID]
	p.mu.Unlock()
	select {
	case <-reaped:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertRetiredStop(t, p, r)
}
func TestUnsupportedPIDFDRefusesBeforeHello(t *testing.T) {
	p, r := placedHost(t, nil)
	before, err := p.Inspect(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(filepath.Dir(r.DescriptorPath), "j")
	snapshot := func() string {
		entries, e := os.ReadDir(journal)
		if e != nil {
			t.Fatal(e)
		}
		var b strings.Builder
		for _, entry := range entries {
			if !entry.IsDir() {
				raw, e := os.ReadFile(filepath.Join(journal, entry.Name()))
				if e != nil {
					t.Fatal(e)
				}
				b.Write(raw)
			}
		}
		return b.String()
	}
	original := getPeerPIDFD
	getPeerPIDFD = func(int) (int, error) { return -1, syscall.ENOPROTOOPT }
	err = p.Stop(context.Background(), r)
	getPeerPIDFD = original
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "unsupported" {
		t.Fatalf("unsupported: %v", err)
	}
	// Health inspection writes journal events, so compare bytes BEFORE inspecting.
	first := snapshot()
	getPeerPIDFD = func(int) (int, error) { return -1, syscall.ENOPROTOOPT }
	err = p.Stop(context.Background(), r)
	getPeerPIDFD = original
	if !errors.As(err, &fault) || fault.Code != "unsupported" {
		t.Fatalf("repeat unsupported: %v", err)
	}
	if snapshot() != first {
		t.Error("unsupported stop wrote journal events")
	}
	after, err := p.Inspect(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if after.Receipt.Epoch != before.Receipt.Epoch {
		t.Error("unsupported stop bumped epoch")
	}
}
func TestSystemdCollectedUnitTeardown(t *testing.T) {
	for _, failed := range []string{"stop", "reset-failed", "offline"} {
		t.Run(failed, func(t *testing.T) {
			p, r := placedHost(t, nil)
			if failed == "offline" {
				handle := testHostHandle(t, r)
				if err := handle.signal(syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := handle.wait(ctx)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			}
			spec, err := Descriptor(r)
			if err != nil {
				t.Fatal(err)
			}
			r.Backend = SystemdUser
			r.UnitName = p.unit(spec)
			t.Cleanup(func() {
				var saved Receipt
				if err := ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); err != nil {
					t.Error(err)
					return
				}
				saved.Backend = Detached
				saved.UnitName = ""
				if err := WritePrivateJSON(metadataPath(r), saved); err != nil {
					t.Error(err)
				}
			})
			p.cfg.AllowSystemd = true
			if err = WritePrivateJSON(metadataPath(r), r); err != nil {
				t.Fatal(err)
			}
			collected := failed == "offline"
			verified := false
			p.cfg.Command = func(_ context.Context, argv []string) ([]byte, error) {
				if argv[2] == "show" {
					if collected {
						verified = true
						return []byte("not-found\n"), nil
					}
					return []byte("loaded\n"), nil
				}
				if argv[2] == failed {
					collected = true
					return nil, errors.New("unit not found")
				}
				return nil, nil
			}
			if err = p.Stop(context.Background(), r); err != nil {
				t.Fatalf("collected unit: %v", err)
			}
			if !verified {
				t.Fatal("unit absence not verified")
			}
			assertRetiredStop(t, p, r)
		})
	}
}
func TestSecretLookingEnvironmentDefaultsVolatile(t *testing.T) {
	cfg, spec := hostSpec(t)
	names := []string{"session_token", "client_SECRET", "API_KEY", "Password", "service_CrEdEnTiAl"}
	for _, key := range names {
		spec.Env = append(spec.Env, key+"=first")
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Place(context.Background(), "secret-retry", spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := p.Stop(context.Background(), r); e != nil {
			t.Error(e)
		}
	})
	for i := range spec.Env {
		key, _, _ := strings.Cut(spec.Env[i], "=")
		for _, name := range names {
			if key == name {
				spec.Env[i] = key + "=re-minted"
			}
		}
	}
	again, err := p.Place(context.Background(), "secret-retry", spec)
	if err != nil || again.Fingerprint != r.Fingerprint {
		t.Fatalf("secret fingerprint changed: %v", err)
	}
	launch, err := Descriptor(again)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range names {
		found := false
		for _, entry := range launch.Env {
			if entry == key+"=first" {
				found = true
			}
		}
		if !found {
			t.Fatalf("original environment missing %s", key)
		}
	}
}
func TestLockWaitReturnsCallerCancellation(t *testing.T) {
	cfg, _ := hostSpec(t)
	path := filepath.Join(cfg.StateDir, "cancel.lock")
	lock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	for _, delayed := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if delayed {
			time.AfterFunc(20*time.Millisecond, cancel)
		} else {
			cancel()
		}
		_, err = LockWait(ctx, path, time.Second)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation (delayed=%t): %v", delayed, err)
		}
	}
}

func TestHostFixtureUsesConfiguredShortTemporaryRoot(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "base-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	t.Setenv("TMPDIR", base)
	_, spec := hostSpec(t)
	if filepath.Dir(spec.Cwd) != base {
		t.Fatal("fixture ignored the configured short temporary root")
	}
}
func TestPIDFDPeerExitRetiresPlacement(t *testing.T) {
	p, r := placedHost(t, func(_ *Config, s *shim.Launch) { s.Env[0] = "TETHER_TEST_SHIM=child-exit" })
	handle := testHostHandle(t, r)
	original := getPeerPIDFD
	getPeerPIDFD = func(int) (int, error) {
		if err := handle.signal(syscall.SIGKILL); err != nil {
			return -1, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := handle.wait(ctx); err != nil {
			return -1, err
		}
		return -1, syscall.ESRCH
	}
	err := p.Stop(context.Background(), r)
	getPeerPIDFD = original
	if err != nil {
		t.Fatalf("peer exited before hello: %v", err)
	}
	assertRetiredStop(t, p, r)
}

func TestCleanupDoesNotSignalAfterSuccessfulStop(t *testing.T) {
	p, r := placedHost(t, nil)
	if err := p.Stop(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	victim := exec.Command(exe, "-test.run=^TestHostProcess$")
	victim.Env = []string{"TETHER_TEST_SHIM=child"}
	in, err := victim.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("victim pid=%d", victim.Process.Pid)
	defer func() { _ = in.Close(); _ = victim.Process.Kill(); _ = victim.Wait() }()
	r.HostPID = victim.Process.Pid
	stopTestPlacement(t, p, r)
	if err := victim.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("cleanup signaled reused PID: %v", err)
	}
	// Signal(0) may see a just-killed zombie; the owned child's pipe must stay open.
	time.Sleep(20 * time.Millisecond)
	if _, err := in.Write([]byte("alive")); err != nil {
		t.Fatalf("cleanup killed unrelated child: %v", err)
	}
}
