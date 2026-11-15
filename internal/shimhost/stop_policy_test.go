//go:build linux

package shimhost

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/shim"
)

func TestStopPolicyChild(t *testing.T) {
	if os.Getenv("TETHER_REAPER_CHILD") != "1" {
		return
	}
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
	if ready := os.Getenv("TETHER_REAPER_READY"); ready != "" {
		_ = os.WriteFile(ready, nil, 0o600)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestStopPolicyEscalatesAndRetires(t *testing.T) {
	var ready string
	p, r := placedHost(t, func(_ *Config, s *shim.Launch) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ready = filepath.Join(s.Cwd, "reaper-ready")
		s.Argv = []string{exe, "-test.run=^TestStopPolicyChild$"}
		s.Env = append(s.Env, "TETHER_REAPER_CHILD=1", "TETHER_REAPER_READY="+ready)
	})
	// A real stubborn child, rather than an implementation-mirroring mock,
	// proves the provider receives each escalating signal and is gone. Stop only
	// once it has said it ignores SIGINT and SIGTERM: a fixed sleep let the
	// request_stop signal kill it first under load (CW-20261115-0074).
	deadline := time.Now().Add(exitWait)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stubborn child never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
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
