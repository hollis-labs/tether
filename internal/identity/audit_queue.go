package identity

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// AuditQueue keeps observation off the request path. Persisted rows are durable;
// overflow, shutdown and persistence failures are counted, never retried forever.
type AuditQueue struct {
	pending  chan Observation
	persist  func(context.Context, Observation) error
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	closed   bool
	dropped  atomic.Uint64
	failures atomic.Uint64
}
type AuditStats struct {
	Dropped  uint64 `json:"dropped"`
	Failures uint64 `json:"failures"`
}

func NewAuditQueue(capacity int, persist func(context.Context, Observation) error) *AuditQueue {
	if capacity < 1 {
		capacity = 1
	}
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: lifetime ownership transfers to AuditQueue; Close cancels and joins the worker.
	q := &AuditQueue{pending: make(chan Observation, capacity), persist: persist, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go q.run()
	return q
}
func (q *AuditQueue) Enqueue(_ context.Context, o Observation) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.dropped.Add(1)
		return nil
	}
	select {
	case q.pending <- o:
	default:
		q.dropped.Add(1)
	}
	return nil
}
func (q *AuditQueue) Stats() AuditStats {
	return AuditStats{Dropped: q.dropped.Load(), Failures: q.failures.Load()}
}
func (q *AuditQueue) Close() {
	q.once.Do(func() { q.mu.Lock(); q.closed = true; q.mu.Unlock(); q.cancel() })
	<-q.done
}
func (q *AuditQueue) run() {
	defer close(q.done)
	for {
		select {
		case <-q.ctx.Done():
			q.dropped.Add(uint64(len(q.pending)))
			return
		case o := <-q.pending:
			ctx, cancel := context.WithTimeout(q.ctx, 5*time.Second)
			if err := q.persist(ctx, o); err != nil {
				q.failures.Add(1)
			}
			cancel()
		}
	}
}
