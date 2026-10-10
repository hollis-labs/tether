package report

import (
	"context"
	"testing"
	"time"
)

func TestSampler(t *testing.T) {
	s := NewSampler(time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{})
	values := make(chan ResourceState)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Start(ctx, func() ResourceState {
			select {
			case calls <- struct{}{}:
			case <-ctx.Done():
				return ResourceState{}
			}
			select {
			case st := <-values:
				return st
			case <-ctx.Done():
				return ResourceState{}
			}
		})
	}()
	t.Cleanup(func() { cancel(); waitSamplerDone(t, done) })
	waitSamplerDone(t, calls)
	values <- ResourceState{CPUCount: 2}
	waitSamplerDone(t, calls) // next measurement waits until this assertion completes
	if got := s.Current().CPUCount; got != 2 {
		t.Fatalf("first CPU count = %d", got)
	}
	values <- ResourceState{CPUCount: 4}
	waitSamplerDone(t, calls)
	if got := s.Current().CPUCount; got != 4 {
		t.Fatalf("next CPU count = %d", got)
	}
}

func waitSamplerDone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("sampler did not make progress")
	}
}

func TestSampler_PanicRecovery(t *testing.T) {
	s := NewSampler(time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Start(context.Background(), func() ResourceState { panic("synthetic panic") })
	}()
	waitSamplerDone(t, done)
	cur := s.Current()
	if cur.Status != "unknown" || cur.CPULoad != "unknown" || cur.MemoryAvailable != nil || cur.StateDiskFree != nil || cur.WorkDiskFree != nil {
		t.Fatalf("panic produced success or numeric zero: %+v", cur)
	}
}

func TestSampler_CancelBeforeFirstMeasurement(t *testing.T) {
	s := NewSampler(time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	measured := false
	s.Start(ctx, func() ResourceState { measured = true; return ResourceState{CPUCount: 1} })
	if measured {
		t.Fatal("sampler should not measure if canceled before first iteration")
	}
	if cur := s.Current(); cur.CPUCount != 0 || cur.Status != "unknown" {
		t.Fatalf("initial state = %+v", cur)
	}
}
