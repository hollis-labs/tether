package report

import (
	"context"
	"sync"
	"time"
)

type Sampler struct {
	mu    sync.RWMutex
	state ResourceState
	delay time.Duration
}

func NewSampler(delay time.Duration) *Sampler {
	if delay == 0 {
		delay = 10 * time.Second
	}
	return &Sampler{
		delay: delay,
		state: ResourceState{Status: "unknown", CPULoad: "unknown"},
	}
}

func (s *Sampler) Start(ctx context.Context, measure func() ResourceState) {
	defer func() {
		if r := recover(); r != nil {
			s.mu.Lock()
			// Explicitly set unknown values
			s.state = ResourceState{
				Status:   "unknown",
				CPUCount: 0,
				CPULoad:  "unknown",
			}
			s.mu.Unlock()
		}
	}()

	select {
	case <-ctx.Done():
		return
	default:
	}

	ticker := time.NewTicker(s.delay)
	defer ticker.Stop()
	for {
		st := measure()
		s.mu.Lock()
		s.state = st
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Sampler) Current() ResourceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}
