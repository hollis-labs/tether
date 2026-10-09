package app

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"
)

// Retry workers accelerate the durable journal. Bound in-memory bodies and
// each worker's retry age; pending records survive expiry, overflow and restart.
type outputRetryState struct {
	mu            sync.Mutex
	wg            sync.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
	closed        bool
	count         int
	active        map[string]bool
	replayStarted bool
	bytes         int
}

func (s *Service) retryTurnOutput(job *turnOutputWrite) {
	s.enqueueOutputRetry(job, false)
}

func (s *Service) enqueueOutputRetry(job *turnOutputWrite, recovered bool) {
	r := &s.outputRetries
	r.mu.Lock()
	if recovered && r.active[job.journalID] {
		r.mu.Unlock()
		return
	}
	if r.closed || r.count >= 64 || (r.count > 0 && r.bytes+len(job.result.Text) > 16*1024*1024) {
		if !recovered {
			delete(r.active, job.journalID)
		}
		r.mu.Unlock()
		log.Printf("ERROR session %q turn %q: output retry unavailable or full", job.row.ID, job.result.TurnID)
		return
	}
	// Permit one large output on its own; never strand it behind the normal
	// aggregate memory budget. Further workers wait in the durable journal.
	if r.ctx == nil {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}
	stop := r.ctx
	if r.active == nil {
		r.active = make(map[string]bool)
	}
	r.active[job.journalID] = true
	r.count++
	r.bytes += len(job.result.Text)
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			r.count--
			r.bytes -= len(job.result.Text)
			delete(r.active, job.journalID)
			r.mu.Unlock()
		}()
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

// Replay starts with the router, before daemon launch traffic, and rescans while
// it runs. Pagination and active claims keep large backlogs and live callbacks
// bounded; an expired/overflowed worker leaves its journal for another sweep.
func (s *Service) startOutputRetryReplay() {
	r := &s.outputRetries
	r.mu.Lock()
	if r.closed || r.replayStarted {
		r.mu.Unlock()
		return
	}
	r.replayStarted = true
	if r.ctx == nil {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}
	ctx := r.ctx
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		cursor := ""
		for {
			ids, err := s.Store.PendingTurnOutputRetries(cursor, 128)
			if err != nil {
				log.Printf("turn output retry journal scan: %v", err)
			}
			for _, id := range ids {
				if ctx.Err() != nil {
					return
				}
				r.mu.Lock()
				active := r.active[id]
				full := r.count >= 64 || r.bytes >= 16*1024*1024
				r.mu.Unlock()
				if full {
					break
				}
				cursor = id
				if active {
					continue
				}
				job, err := readOutputRetry(s, id)
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						log.Printf("turn output retry journal read %q: %v", id, err)
					}
					continue
				}
				s.enqueueOutputRetry(job, true)
			}
			if len(ids) < 128 {
				cursor = ""
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
