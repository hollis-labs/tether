package app

import (
	"context"
	"log"
	"sync"
	"time"
)

// Before staging, retries are necessarily volatile. Bound both retained bodies
// and retry age; after staging the router's durable scan survives a restart.
type outputRetryState struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	count  int
	bytes  int
}

func (s *Service) retryTurnOutput(job *turnOutputWrite) {
	r := &s.outputRetries
	r.mu.Lock()
	if r.closed || r.count >= 64 || r.bytes+len(job.result.Text) > 16*1024*1024 {
		r.mu.Unlock()
		log.Printf("ERROR session %q turn %q: output retry unavailable or full", job.row.ID, job.result.TurnID)
		return
	}
	if r.ctx == nil {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}
	stop := r.ctx
	r.count++
	r.bytes += len(job.result.Text)
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer func() { r.mu.Lock(); r.count--; r.bytes -= len(job.result.Text); r.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(stop, time.Minute)
		defer cancel()
		delay := 100 * time.Millisecond
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				if stop.Err() != nil {
					// Shutdown cancels an in-flight attempt, then gives the
					// retained output one fresh, bounded attempt before joining.
					final, finish := s.outputPersistenceContext()
					err := job.persist(final, s)
					finish()
					if err != nil {
						log.Printf("ERROR session %q turn %q: final output retry failed (staged message %q): %v", job.row.ID, job.result.TurnID, job.messageID, err)
					}
				} else {
					log.Printf("ERROR session %q turn %q: output retry expired (staged message %q)", job.row.ID, job.result.TurnID, job.messageID)
				}
				return
			case <-timer.C:
				if job.persist(ctx, s) == nil {
					return
				}
				delay = min(delay*2, 5*time.Second)
				timer.Reset(delay)
			}
		}
	}()
}

// Called after readers and flush have finished, before router/store close.
// Join even a currently blocked attempt, whose operations each have a budget.
func (s *Service) stopOutputRetries() {
	r := &s.outputRetries
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		if r.cancel != nil {
			r.cancel()
		}
	}
	r.mu.Unlock()
	r.wg.Wait()
}
