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
	stop   chan struct{}
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
	if r.stop == nil {
		r.stop = make(chan struct{})
	}
	stop := r.stop
	r.count++
	r.bytes += len(job.result.Text)
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer func() { r.mu.Lock(); r.count--; r.bytes -= len(job.result.Text); r.mu.Unlock() }()
		deadline := time.NewTimer(time.Minute)
		defer deadline.Stop()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				log.Printf("ERROR session %q turn %q: output retry stopped at shutdown (staged message %q)", job.row.ID, job.result.TurnID, job.messageID)
				return
			case <-deadline.C:
				log.Printf("ERROR session %q turn %q: output retry expired (staged message %q)", job.row.ID, job.result.TurnID, job.messageID)
				return
			case <-ticker.C:
				if job.persist(context.Background(), s) == nil {
					return
				}
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
		if r.stop != nil {
			close(r.stop)
		}
	}
	r.mu.Unlock()
	r.wg.Wait()
}
