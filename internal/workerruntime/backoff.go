package workerruntime

import (
	"math/rand/v2"
	"time"
)

const (
	reconnectBase   = time.Second
	reconnectMax    = 15 * time.Second
	reconnectJitter = 0.2
	// reconnectStable is how long a welcomed connection must stay up before the
	// delay resets; a link that drops right after welcome keeps growing it.
	reconnectStable = 30 * time.Second
)

// reconnectBackoff is the worker's reconnect schedule: doubling from 1s to 15s,
// +-20% jitter so workers do not reconnect in lockstep after a hub restart, and
// a reset only after a stable connection.
type reconnectBackoff struct {
	cur  time.Duration
	nowF func() time.Time
	rnd  func() float64
}

func newReconnectBackoff(now func() time.Time, rnd func() float64) *reconnectBackoff {
	if now == nil {
		now = time.Now
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	return &reconnectBackoff{cur: reconnectBase, nowF: now, rnd: rnd}
}

func (b *reconnectBackoff) now() time.Time { return b.nowF() }

// connectionEnded resets the schedule when the connection stayed up long enough.
func (b *reconnectBackoff) connectionEnded(up time.Duration) {
	if up >= reconnectStable {
		b.cur = reconnectBase
	}
}

// next returns the jittered delay to wait now and doubles the base for the next call.
func (b *reconnectBackoff) next() time.Duration {
	d := time.Duration(float64(b.cur) * (1 + reconnectJitter*(2*b.rnd()-1)))
	b.cur = min(reconnectMax, b.cur*2)
	return d
}
