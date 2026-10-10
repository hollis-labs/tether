package report

import (
	"context"
	"testing"
	"time"
)

func TestSampler(t *testing.T) {
	s := NewSampler(10 * time.Millisecond)
	ch := make(chan ResourceState, 1)
	ch <- ResourceState{CPUCount: 2}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.Start(ctx, func() ResourceState {
		select {
		case st := <-ch:
			return st
		default:
			return ResourceState{CPUCount: 4}
		}
	})

	time.Sleep(5 * time.Millisecond)
	cur := s.Current()
	if cur.CPUCount != 2 {
		t.Errorf("expected 2, got %d", cur.CPUCount)
	}

	time.Sleep(15 * time.Millisecond)
	cur = s.Current()
	if cur.CPUCount != 4 {
		t.Errorf("expected 4, got %d", cur.CPUCount)
	}
}

func TestSampler_PanicRecovery(t *testing.T) {
	s := NewSampler(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.Start(ctx, func() ResourceState {
		panic("test panic")
	})

	time.Sleep(20 * time.Millisecond)
	// Should not crash the test suite
}

func TestSampler_CancelBeforeFirstMeasurement(t *testing.T) {
	s := NewSampler(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	measured := false
	s.Start(ctx, func() ResourceState {
		measured = true
		return ResourceState{CPUCount: 1}
	})

	if measured {
		t.Errorf("sampler should not measure if cancelled before first iteration")
	}

	cur := s.Current()
	if cur.CPUCount != 0 {
		t.Errorf("expected empty state")
	}
}
