//go:build linux

package shimhost

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
)

func retainedCapability(t *testing.T, r Receipt) {
	t.Helper()
	var saved Receipt
	if err := ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); err != nil || saved.Retired {
		t.Errorf("placement retired: %+v %v", saved, err)
	}
	if _, err := os.Stat(r.DescriptorPath); err != nil {
		t.Errorf("capability removed: %v", err)
	}
}
func restorePlacement(t *testing.T, r Receipt) {
	t.Helper()
	spec, err := Descriptor(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = WritePrivateJSON(r.DescriptorPath, spec); _ = WritePrivateJSON(metadataPath(r), r) })
}
func TestInspectRefusalDoesNotPoisonReceipt(t *testing.T) {
	for _, reattach := range []bool{false, true} {
		t.Run(map[bool]string{false: "inspect", true: "reattach"}[reattach], func(t *testing.T) {
			p, r := placedHost(t, nil)
			restorePlacement(t, r)
			saved := r
			saved.HostPID = 0
			saved.Journal = ""
			if err := WritePrivateJSON(metadataPath(r), saved); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(metadataPath(r))
			if err != nil {
				t.Fatal(err)
			}
			stale := r
			stale.HostPID = exitedTestPID(t)
			stale.Journal = "wrong-journal"
			var observed Inspection
			if reattach {
				observed, err = p.Reattach(context.Background(), stale)
			} else {
				observed, err = p.Inspect(context.Background(), stale)
			}
			var fault *Failure
			if !errors.As(err, &fault) || fault.Code != "identity_mismatch" || observed.Gone {
				t.Errorf("decisive refusal: %+v %v", observed, err)
			}
			after, e := os.ReadFile(metadataPath(r))
			if e != nil || string(after) != string(before) {
				t.Error("refusal changed durable receipt")
			}
		})
	}
}
func TestReapedEntryCannotRetireLiveIdentity(t *testing.T) {
	p, r := placedHost(t, nil)
	restorePlacement(t, r)
	hidden := r.SocketPath + ".hidden"
	if err := os.Rename(r.SocketPath, hidden); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(hidden, r.SocketPath) })
	closed := make(chan struct{})
	close(closed)
	p.mu.Lock()
	p.reaped[identity(r)] = closed
	p.mu.Unlock()
	// The waiter entry must never replace a fresh check of the recorded identity.
	err := p.Stop(context.Background(), r)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "outcome_unknown" {
		t.Errorf("live identity: %v", err)
	}
	retainedCapability(t, r)
}
func TestSystemdLoadedUnitStopIsRetried(t *testing.T) {
	p, r := placedHost(t, nil)
	restorePlacement(t, r)
	spec, err := Descriptor(r)
	if err != nil {
		t.Fatal(err)
	}
	original := r
	r.Backend = SystemdUser
	r.UnitName = p.unit(spec)
	p.cfg.AllowSystemd = true
	if err = WritePrivateJSON(metadataPath(r), r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var saved Receipt
		_ = ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved)
		saved.Backend = original.Backend
		saved.UnitName = ""
		_ = WritePrivateJSON(metadataPath(r), saved)
	})
	attempts := 0
	gone := false
	stopErr := errors.New("unit still loaded")
	p.cfg.Command = func(_ context.Context, argv []string) ([]byte, error) {
		switch argv[2] {
		case "show":
			if gone {
				return []byte("not-found"), nil
			}
			return []byte("loaded"), nil
		case "stop":
			attempts++
			if attempts == 1 {
				return nil, stopErr
			}
			gone = true
		}
		return nil, nil
	}
	if err = p.Stop(context.Background(), r); !errors.Is(err, stopErr) {
		t.Errorf("first stop: %v", err)
	}
	retainedCapability(t, r)
	if err = p.Stop(context.Background(), r); err != nil {
		t.Errorf("retry stop: %v", err)
	}
	if attempts != 2 {
		t.Errorf("stop attempts: %d", attempts)
	}
	assertRetiredStop(t, p, r)
}
func TestFailedSpawnRetiresTerminalPlacement(t *testing.T) {
	cfg, spec := hostSpec(t)
	cfg.ShimCommand = []string{filepath.Join(spec.Cwd, "missing-shim")}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Place(context.Background(), "failed-spawn", spec)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "placement_failed" || !r.Retired {
		t.Errorf("failed spawn: %+v %v", r, err)
	}
	assertRetiredStop(t, p, r)
	again, err := p.Place(context.Background(), "failed-spawn", spec)
	if !errors.As(err, &fault) || fault.Code != "placement_failed" || !again.Retired {
		t.Errorf("terminal retry: %+v %v", again, err)
	}
}
func TestStopCommitsAdoptedIdentityBeforeEffects(t *testing.T) {
	p, r := placedHost(t, nil)
	restorePlacement(t, r)
	missing := r
	missing.HostStartTime = 0
	if err := WritePrivateJSON(metadataPath(r), missing); err != nil {
		t.Fatal(err)
	}
	lock, err := Lock(filepath.Join(filepath.Dir(r.DescriptorPath), "record.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(context.Background(), missing) }()
	time.Sleep(100 * time.Millisecond)
	spec, err := Descriptor(r)
	if err != nil {
		t.Error(err)
	} else {
		c, e := Connect(context.Background(), r.SocketPath, spec.Secret, r.Session, r.Instance, "1", "observer", r.Journal, false)
		if e != nil {
			t.Errorf("host signaled before witness commit: %v", e)
		} else {
			running, _, _, e := health(context.Background(), c, r.Session)
			_ = c.Close()
			if e != nil || !running {
				t.Errorf("provider killed before witness commit: %v", e)
			}
		}
	}
	_ = lock.Close()
	if err = <-stopped; err != nil {
		t.Error(err)
	}
	var saved Receipt
	if err = ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); err != nil || saved.HostStartTime != r.HostStartTime {
		t.Errorf("adopted identity not durable: %+v %v", saved, err)
	}
}
func TestRetirementCommitPrecedesCapabilityCleanup(t *testing.T) {
	p, r := placedHost(t, nil)
	lock, err := Lock(filepath.Join(filepath.Dir(r.DescriptorPath), "record.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(context.Background(), r) }()
	// Wait for the owned host's exit; the record writer must now be blocked.
	p.mu.Lock()
	reaped := p.reaped[identity(r)]
	p.mu.Unlock()
	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Error("host did not exit")
	}
	time.Sleep(30 * time.Millisecond)
	retainedCapability(t, r)
	_ = lock.Close()
	if err = <-stopped; err != nil {
		t.Error(err)
	}
	assertRetiredStop(t, p, r)
}
func TestConnectedStartTimeMismatchRefusesBeforeEffects(t *testing.T) {
	p, r := placedHost(t, nil)
	restorePlacement(t, r)
	wrong := r
	wrong.HostStartTime++
	if err := WritePrivateJSON(metadataPath(r), wrong); err != nil {
		t.Fatal(err)
	}
	err := p.Stop(context.Background(), wrong)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "identity_mismatch" {
		t.Errorf("connected identity: %v", err)
	}
	retainedCapability(t, r)
}
func TestRecordedReplacedStartTimeProvesAbsence(t *testing.T) {
	start, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !recordedIdentityGone(os.Getpid(), start+1) {
		t.Error("replaced identity ignored")
	}
	if recordedIdentityGone(os.Getpid(), start) {
		t.Error("live identity treated as absent")
	}
}
func TestConnectionFailureExcludesTimeoutAndProtocolErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		absent bool
	}{
		{"refused", &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}, true},
		{"missing", &net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}, true},
		{"dial timeout", &net.OpError{Op: "dial", Net: "unix", Err: context.DeadlineExceeded}, false},
		{"read timeout", &net.OpError{Op: "read", Net: "unix", Err: context.DeadlineExceeded}, false},
		{"timeout", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"closed hello", io.EOF, false},
		{"refusal", fail("identity_mismatch", "hello refused"), false},
		{"other error", errors.New("unknown transport failure"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if connectionFailure(c.err) != c.absent {
				t.Errorf("absence classification: %v", c.err)
			}
		})
	}
}
func TestInspectGoneDoesNotMergeCallerFacts(t *testing.T) {
	p, r := placedHost(t, nil)
	handle := testHostHandle(t, r)
	if err := handle.signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handle.wait(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(metadataPath(r))
	if err != nil {
		t.Fatal(err)
	}
	caller := r
	caller.Epoch = "999"
	caller.ProviderPID = 0
	observed, err := p.Inspect(context.Background(), caller)
	if err != nil || !observed.Gone {
		t.Fatalf("gone inspection: %+v %v", observed, err)
	}
	after, err := os.ReadFile(metadataPath(r))
	if err != nil || string(before) != string(after) {
		t.Error("gone result changed receipt")
	}
}
func TestStopWithoutRecordedPIDRefusesLivePeer(t *testing.T) {
	p, r := placedHost(t, nil)
	restorePlacement(t, r)
	missing := r
	missing.HostPID = 0
	missing.ShimPID = 0
	missing.HostStartTime = 0
	if err := WritePrivateJSON(metadataPath(r), missing); err != nil {
		t.Fatal(err)
	}
	err := p.Stop(context.Background(), missing)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "outcome_unknown" {
		t.Errorf("missing PID: %v", err)
	}
	retainedCapability(t, r)
}
func TestInspectForeignUnitRefusesBeforeCommand(t *testing.T) {
	p, r := placedHost(t, nil)
	restorePlacement(t, r)
	r.Backend = SystemdUser
	r.UnitName = "foreign.service"
	p.cfg.AllowSystemd = true
	if err := WritePrivateJSON(metadataPath(r), r); err != nil {
		t.Fatal(err)
	}
	called := false
	p.cfg.Command = func(context.Context, []string) ([]byte, error) { called = true; return []byte("not-found"), nil }
	_, err := p.Inspect(context.Background(), r)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "identity_mismatch" || called {
		t.Errorf("foreign unit inspection: %v called=%t", err, called)
	}
}

func TestStopNonConnectionFailureRetainsGoneHost(t *testing.T) {
	for _, kind := range []string{"timeout", "other"} {
		t.Run(kind, func(t *testing.T) {
			p, r := placedHost(t, nil)
			handle := testHostHandle(t, r)
			if err := handle.signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := handle.wait(ctx); err != nil {
				t.Fatal(err)
			}
			original := dialControl
			transportErr := context.DeadlineExceeded
			if kind == "other" {
				transportErr = errors.New("unknown dial failure")
			}
			dialControl = func(context.Context, string) (net.Conn, error) {
				return nil, &net.OpError{Op: "dial", Net: "unix", Err: transportErr}
			}
			err := p.Stop(context.Background(), r)
			dialControl = original
			if !errors.Is(err, transportErr) {
				t.Errorf("nonconnection failure changed: %v", err)
			}
			retainedCapability(t, r)
		})
	}
}
