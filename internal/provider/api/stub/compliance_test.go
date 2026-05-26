package stub

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/agentkit/agentsessions/compliance"

	"github.com/hollis-labs/tether/internal/launch"
)

// TestCompliance runs the shared go-agent-sessions compliance suite against
// the api-stub Runtime. The stub is in-process (no external binary) so all
// caps tests that gate on BinaryRequired=false also run.
func TestCompliance(t *testing.T) {
	rt, err := New(&launch.Plan{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	compliance.Run(t, compliance.Harness{
		Runtime: rt,
		NewStartOptions: func(t *testing.T) agentsessions.StartOptions {
			return agentsessions.StartOptions{Workdir: t.TempDir()}
		},
	})
}

func TestStub_StartProducesAliveSession(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	sess, err := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h := sess.Health(); !h.Alive || h.PID != 0 {
		t.Errorf("Health = %+v, want Alive=true PID=0", h)
	}
}

func TestStub_SendInputEchoesThroughFanout(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	var buf threadsafeBuffer
	sess, err := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: t.TempDir(), Fanout: &buf})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sess.SendInput(context.Background(), []byte("hello")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if got := buf.String(); got != "echo: hello\n" {
		t.Errorf("fanout = %q, want %q", got, "echo: hello\n")
	}
}

func TestStub_SendInputPreservesExplicitNewline(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	var buf threadsafeBuffer
	sess, _ := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: t.TempDir(), Fanout: &buf})
	if err := sess.SendInput(context.Background(), []byte("hi\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if got := buf.String(); got != "echo: hi\n" {
		t.Errorf("fanout = %q, want %q (no doubled newline)", got, "echo: hi\n")
	}
}

func TestStub_StopUnblocksWait(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	sess, _ := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: t.TempDir()})
	done := make(chan int, 1)
	go func() {
		code, _ := sess.Wait()
		done <- code
	}()

	select {
	case <-done:
		t.Fatal("Wait returned before Stop was called")
	case <-time.After(50 * time.Millisecond):
	}

	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("Wait returned code %d, want 0", code)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Wait did not return after Stop")
	}

	if h := sess.Health(); h.Alive {
		t.Error("Health.Alive should be false after Stop")
	}
}

func TestStub_SendInputAfterStopErrors(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	sess, _ := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: t.TempDir()})
	_ = sess.Stop(context.Background())
	err := sess.SendInput(context.Background(), []byte("too late"))
	if !errors.Is(err, agentsessions.ErrNoInputChannel) {
		t.Errorf("expected agentsessions.ErrNoInputChannel; got %v", err)
	}
}

func TestStub_StopIsIdempotent(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	sess, _ := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: t.TempDir()})
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStub_BootPromptEchoesWhenModeStdin(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	var buf threadsafeBuffer
	_, err := rt.Start(context.Background(), agentsessions.StartOptions{
		Workdir:    t.TempDir(),
		Fanout:     &buf,
		BootPrompt: "boot",
		BootMode:   "stdin",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := buf.String(); got != "echo: boot\n" {
		t.Errorf("boot prompt not echoed; fanout = %q", got)
	}
}

func TestStub_BootPromptSilentWhenModeNotStdin(t *testing.T) {
	rt, _ := New(&launch.Plan{})
	var buf threadsafeBuffer
	_, err := rt.Start(context.Background(), agentsessions.StartOptions{
		Workdir:    t.TempDir(),
		Fanout:     &buf,
		BootPrompt: "boot",
		BootMode:   "",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("non-stdin mode should not echo boot prompt; fanout = %q", buf.String())
	}
}

// threadsafeBuffer is a minimal sync-wrapped byte buffer for test Fanout use.
type threadsafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *threadsafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *threadsafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
