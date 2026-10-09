//go:build linux

package shimhost

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
)

func TestStopPolicyChild(t *testing.T) {
	if os.Getenv("TETHER_REAPER_CHILD") != "1" {
		return
	}
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
	for {
		time.Sleep(time.Hour)
	}
}

func TestStopPolicyEscalatesAndRetires(t *testing.T) {
	p, r := placedHost(t, func(_ *Config, s *shim.Launch) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		s.Argv = []string{exe, "-test.run=^TestStopPolicyChild$"}
		s.Env = append(s.Env, "TETHER_REAPER_CHILD=1")
	})
	// A real stubborn child, rather than an implementation-mirroring mock,
	// proves the provider receives each escalating signal and is gone.
	time.Sleep(100 * time.Millisecond)
	var stages []string
	err := p.StopWithPolicy(context.Background(), r, StopPolicy{
		RequestGrace: 50 * time.Millisecond, TerminateGrace: 50 * time.Millisecond, KillGrace: time.Second,
		BeforeStage: func(_ context.Context, stage string) error { stages = append(stages, stage); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stages, []string{"request_stop", "terminate", "kill"}) {
		t.Fatalf("stages=%v", stages)
	}
	assertRetiredStop(t, p, r)
}

func TestStopPolicyAuditFailurePreservesPlacement(t *testing.T) {
	p, r := placedHost(t, nil)
	refused := errors.New("audit unavailable")
	err := p.StopWithPolicy(context.Background(), r, StopPolicy{RequestGrace: time.Millisecond, BeforeStage: func(context.Context, string) error { return refused }})
	if !errors.Is(err, refused) {
		t.Fatalf("stop=%v", err)
	}
	got, err := p.Inspect(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Running {
		t.Fatal("provider stopped despite audit failure")
	}
	if _, err := Descriptor(r); err != nil {
		t.Fatal("placement discarded")
	}
}
