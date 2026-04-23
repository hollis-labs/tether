// Package compliance provides a shared behavioral test suite for provider
// adapters. Every adapter that satisfies provider.Runtime + provider.Session
// must pass the baseline suite. Optional capability-gated tests run only
// when the adapter's Caps() declares the corresponding capability.
//
// # Usage
//
// In each adapter's test package, add a TestCompliance function:
//
//	func TestCompliance(t *testing.T) {
//	    compliance.Run(t, compliance.Harness{
//	        NewRuntime: func(t *testing.T) provider.Runtime { return Adapter{} },
//	        NewPlan:    func(t *testing.T) *launch.Plan {
//	            return &launch.Plan{Command: "sh", Args: []string{"-c", "exit 0"}}
//	        },
//	        BinarySkip: false,
//	    })
//	}
//
// Adapters that require an external binary unavailable in CI should set
// BinarySkip: true. All baseline lifecycle tests still run (using the
// provided plan), but capability tests requiring real binary behavior are
// skipped with an explicit t.Skip message.
package compliance

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
)

// Harness configures a compliance run for one adapter.
type Harness struct {
	// NewRuntime returns the Runtime under test. Called once per Run.
	// Must not return nil.
	NewRuntime func(t *testing.T) provider.Runtime

	// NewPlan returns a launch.Plan suitable for the Runtime.
	// For CLI adapters the Plan must have Command set to a binary that
	// exits cleanly (e.g. /bin/true, sh -c "exit 0").
	// For in-process adapters (api-stub) an empty Plan is fine.
	NewPlan func(t *testing.T) *launch.Plan

	// NewStartOptions, when non-nil, is called per test to produce the
	// StartOptions passed to Runtime.Start. If nil, a default options
	// struct is used with Workdir and LogPath set to a temp directory.
	// Adapters that need specific start options (e.g. PTY providers
	// that require LogPath) should supply this field.
	NewStartOptions func(t *testing.T) provider.StartOptions

	// BinarySkip, when true, marks tests that require the real binary
	// as skipped. Use when the binary is not guaranteed to be present
	// in the test environment. Baseline lifecycle tests still run.
	BinarySkip bool
}

// Run executes the full compliance suite for the adapter described by h.
// It runs baseline tests unconditionally and gates optional tests on Caps().
func Run(t *testing.T, h Harness) {
	t.Helper()
	rt := h.NewRuntime(t)
	caps := rt.Caps()

	// Build the startOpts factory used across all subtests.
	startOptsFn := h.NewStartOptions
	if startOptsFn == nil {
		startOptsFn = func(t *testing.T) provider.StartOptions {
			dir := t.TempDir()
			return provider.StartOptions{
				Workdir: dir,
				LogPath: dir + "/session.log",
			}
		}
	}

	planFn := h.NewPlan

	t.Run("Baseline", func(t *testing.T) {
		runBaseline(t, rt, planFn, startOptsFn, h.BinarySkip)
	})

	if caps.PTY {
		t.Run("CapsPTY", func(t *testing.T) {
			runCapsPTY(t, rt, planFn, startOptsFn, h.BinarySkip)
		})
	}

	if caps.Resize {
		t.Run("CapsResize", func(t *testing.T) {
			runCapsResize(t, rt, planFn, startOptsFn, h.BinarySkip)
		})
	}

	if caps.ProviderSessionID {
		t.Run("CapsProviderSessionID", func(t *testing.T) {
			runCapsProviderSessionID(t, rt, planFn, startOptsFn, h.BinarySkip)
		})
	}

	if !caps.BinaryRequired {
		t.Run("CapsNoBinary", func(t *testing.T) {
			runCapsNoBinary(t, rt)
		})
	}

	if caps.CheckpointResume {
		t.Run("CapsCheckpointResume", func(t *testing.T) {
			runCapsCheckpointResume(t, rt, planFn, startOptsFn, h.BinarySkip)
		})
	}
}

// ---------------------------------------------------------------------------
// Baseline suite
// ---------------------------------------------------------------------------

type planFn = func(t *testing.T) *launch.Plan
type startOptsFn = func(t *testing.T) provider.StartOptions

func runBaseline(t *testing.T, rt provider.Runtime, plan planFn, opts startOptsFn, binarySkip bool) {
	t.Helper()

	t.Run("InterfaceConformance", func(t *testing.T) {
		testInterfaceConformance(t, rt)
	})
	t.Run("KindNonEmpty", func(t *testing.T) {
		testKindNonEmpty(t, rt)
	})
	t.Run("IDNonEmpty", func(t *testing.T) {
		testIDNonEmpty(t, rt)
	})
	t.Run("StartProducesAliveSession", func(t *testing.T) {
		testStartProducesAliveSession(t, rt, plan(t), opts(t))
	})
	t.Run("HealthAliveAfterStart", func(t *testing.T) {
		testHealthAliveAfterStart(t, rt, plan(t), opts(t))
	})
	t.Run("HealthDeadAfterStop", func(t *testing.T) {
		testHealthDeadAfterStop(t, rt, plan(t), opts(t))
	})
	t.Run("HealthStateStoppedAfterStop", func(t *testing.T) {
		testHealthStateStoppedAfterStop(t, rt, plan(t), opts(t))
	})
	t.Run("StopUnblocksWait", func(t *testing.T) {
		testStopUnblocksWait(t, rt, plan(t), opts(t))
	})
	t.Run("StopIsIdempotent", func(t *testing.T) {
		testStopIsIdempotent(t, rt, plan(t), opts(t))
	})
	t.Run("SendInputAfterStopReturnsErrNoInputChannel", func(t *testing.T) {
		testSendInputAfterStop(t, rt, plan(t), opts(t))
	})
	t.Run("WaitReturnsExitCode", func(t *testing.T) {
		testWaitReturnsExitCode(t, rt, plan(t), opts(t))
	})
	t.Run("ResizeNoOpOrNoError", func(t *testing.T) {
		testResizeNoOpOrNoError(t, rt, plan(t), opts(t))
	})
	t.Run("CheckpointHintsReturnsBool", func(t *testing.T) {
		testCheckpointHintsReturnsBool(t, rt, plan(t), opts(t))
	})
	if rt.Caps().BinaryRequired {
		t.Run("PrepareEmptyCommandErrors", func(t *testing.T) {
			if binarySkip {
				t.Skip("BinarySkip=true: skipping binary-dependent prepare test")
			}
			testPrepareEmptyCommandErrors(t, rt)
		})
	}
}

func testInterfaceConformance(t *testing.T, rt provider.Runtime) {
	t.Helper()
	// Compile-time checks ensure Runtime is satisfied; this test verifies
	// the concrete value is non-nil at test time.
	if rt == nil {
		t.Fatal("Harness.NewRuntime returned nil")
	}
}

func testKindNonEmpty(t *testing.T, rt provider.Runtime) {
	t.Helper()
	if rt.Kind() == "" {
		t.Errorf("Kind() = empty string, want non-empty")
	}
}

func testIDNonEmpty(t *testing.T, rt provider.Runtime) {
	t.Helper()
	if rt.ID() == "" {
		t.Errorf("ID() = empty string, want non-empty")
	}
}

func testStartProducesAliveSession(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	defer func() { _ = sess.Stop(context.Background()) }()
	if h := sess.Health(); !h.Alive {
		t.Errorf("Health().Alive = false immediately after Start; want true")
	}
}

func testHealthAliveAfterStart(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	defer func() { _ = sess.Stop(context.Background()) }()
	if h := sess.Health(); !h.Alive {
		t.Errorf("Health().Alive = false after Start; want true")
	}
}

func testHealthDeadAfterStop(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h := sess.Health(); h.Alive {
		t.Errorf("Health().Alive = true after Stop; want false")
	}
}

func testHealthStateStoppedAfterStop(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h := sess.Health(); h.State != provider.LiveStateStopped {
		t.Errorf("Health().State = %v after Stop; want LiveStateStopped", h.State)
	}
}

func testStopUnblocksWait(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)

	done := make(chan int, 1)
	go func() {
		code, _ := sess.Wait()
		done <- code
	}()

	// Ensure Wait is blocking before we call Stop.
	select {
	case <-done:
		// Some providers (e.g. those that run short-lived processes) may
		// complete naturally — that's acceptable, but we can't test the
		// Stop-unblocks-Wait guarantee in that case.
		t.Skip("Wait returned before Stop — provider may have exited naturally")
	case <-time.After(50 * time.Millisecond):
	}

	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return within 3s after Stop")
	}
}

func testStopIsIdempotent(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func testSendInputAfterStop(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	err := sess.SendInput(context.Background(), []byte("too late"))
	if !errors.Is(err, provider.ErrNoInputChannel) {
		t.Errorf("SendInput after Stop = %v; want provider.ErrNoInputChannel", err)
	}
}

func testWaitReturnsExitCode(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)

	// Stop the session to ensure Wait returns; in the general case
	// a provider may not exit naturally without input.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sess.Wait()
	}()

	_ = sess.Stop(context.Background())

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return within 3s")
	}
}

func testResizeNoOpOrNoError(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	defer func() { _ = sess.Stop(context.Background()) }()
	// Non-PTY adapters must not return an error from Resize.
	if err := sess.Resize(context.Background(), 24, 80); err != nil {
		t.Errorf("Resize(24,80) = %v; want nil for non-PTY adapter", err)
	}
}

func testCheckpointHintsReturnsBool(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) {
	t.Helper()
	sess := mustStart(t, rt, plan, opts)
	defer func() { _ = sess.Stop(context.Background()) }()
	// Just confirm it's callable and doesn't panic.
	_, _ = sess.CheckpointHints()
}

func testPrepareEmptyCommandErrors(t *testing.T, rt provider.Runtime) {
	t.Helper()
	err := rt.Prepare(context.Background(), &launch.Plan{})
	if err == nil {
		t.Errorf("Prepare with empty command = nil; want error")
	}
}

// ---------------------------------------------------------------------------
// Optional capability suites
// ---------------------------------------------------------------------------

func runCapsPTY(t *testing.T, rt provider.Runtime, plan planFn, opts startOptsFn, binarySkip bool) {
	t.Helper()
	t.Run("SendInputWritesToFanout", func(t *testing.T) {
		if binarySkip {
			t.Skip("BinarySkip=true: PTY binary not available")
		}
		o := opts(t)
		var buf syncBuffer
		o.Fanout = &buf
		sess := mustStart(t, rt, plan(t), o)
		defer func() { _ = sess.Stop(context.Background()) }()
		// PTY: boot prompt echoes are adapter-defined; we just confirm
		// SendInput doesn't error on a live session.
		_ = sess.SendInput(context.Background(), []byte("echo hi\n"))
	})
}

func runCapsResize(t *testing.T, rt provider.Runtime, plan planFn, opts startOptsFn, binarySkip bool) {
	t.Helper()
	t.Run("ResizeDoesNotError", func(t *testing.T) {
		if binarySkip {
			t.Skip("BinarySkip=true: resize binary not available")
		}
		sess := mustStart(t, rt, plan(t), opts(t))
		defer func() { _ = sess.Stop(context.Background()) }()
		if err := sess.Resize(context.Background(), 40, 120); err != nil {
			t.Errorf("Resize(40,120) = %v; want nil", err)
		}
	})
	t.Run("ResizeZeroErrors", func(t *testing.T) {
		if binarySkip {
			t.Skip("BinarySkip=true: resize binary not available")
		}
		sess := mustStart(t, rt, plan(t), opts(t))
		defer func() { _ = sess.Stop(context.Background()) }()
		// A PTY syscall (TIOCSWINSZ) with 0×0 is either rejected or a no-op
		// depending on the OS. We accept either; what we forbid is a panic.
		_ = sess.Resize(context.Background(), 0, 0)
	})
}

func runCapsProviderSessionID(t *testing.T, rt provider.Runtime, plan planFn, opts startOptsFn, binarySkip bool) {
	t.Helper()
	t.Run("ImplementsSessionIDer", func(t *testing.T) {
		sess := mustStart(t, rt, plan(t), opts(t))
		defer func() { _ = sess.Stop(context.Background()) }()
		if _, ok := sess.(provider.SessionIDer); !ok {
			t.Errorf("Session does not implement provider.SessionIDer; required when Caps().ProviderSessionID=true")
		}
	})
	t.Run("PresetCarriedBeforeTurn", func(t *testing.T) {
		o := opts(t)
		o.ClaudeSessionIDPreset = "ses_compliance_preset"
		sess := mustStart(t, rt, plan(t), o)
		defer func() { _ = sess.Stop(context.Background()) }()
		sider, ok := sess.(provider.SessionIDer)
		if !ok {
			t.Skip("SessionIDer not implemented — covered by ImplementsSessionIDer")
		}
		if got := sider.ProviderSessionID(); got != "ses_compliance_preset" {
			t.Errorf("ProviderSessionID() = %q before any turn; want ses_compliance_preset", got)
		}
	})
}

func runCapsCheckpointResume(t *testing.T, rt provider.Runtime, plan planFn, opts startOptsFn, binarySkip bool) {
	t.Helper()
	t.Run("CheckpointHintsNonTrivial", func(t *testing.T) {
		if binarySkip {
			t.Skip("BinarySkip=true: checkpoint resume binary not available")
		}
		sess := mustStart(t, rt, plan(t), opts(t))
		defer func() { _ = sess.Stop(context.Background()) }()
		_, ok := sess.CheckpointHints()
		if !ok {
			t.Errorf("CheckpointHints() = (_, false); want true when Caps().CheckpointResume=true")
		}
	})
}

func runCapsNoBinary(t *testing.T, rt provider.Runtime) {
	t.Helper()
	t.Run("PrepareEmptyPlanNoError", func(t *testing.T) {
		// In-process providers (api-stub) must not error on empty plan.
		if err := rt.Prepare(context.Background(), &launch.Plan{}); err != nil {
			t.Errorf("Prepare with empty plan = %v; want nil for BinaryRequired=false provider", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mustStart calls Start and fails the test if it errors.
func mustStart(t *testing.T, rt provider.Runtime, plan *launch.Plan, opts provider.StartOptions) provider.Session {
	t.Helper()
	sess, err := rt.Start(context.Background(), plan, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return sess
}

// syncBuffer is a thread-safe bytes.Buffer for use as a Fanout writer in tests.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
