package eventingest

import (
	"sync"
	"time"
)

// limiter is a token bucket per caller key.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate float64, burst int, now func() time.Time) *limiter {
	return &limiter{rate: rate, burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

// take removes n tokens and returns 0, or returns how long to wait and takes
// nothing. A request larger than the burst is allowed once the bucket is full,
// so a single large batch can never be refused forever.
func (l *limiter) take(key string, n int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
		if len(l.buckets) > 4096 { // bound memory: forget idle callers
			for k, v := range l.buckets {
				if now.Sub(v.at) > time.Minute {
					delete(l.buckets, k)
				}
			}
		}
	}
	b.tokens += now.Sub(b.at).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.at = now
	need := float64(n)
	if need > l.burst {
		need = l.burst
	}
	if b.tokens < need {
		wait := time.Duration((need - b.tokens) / l.rate * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return wait
	}
	b.tokens -= need
	return 0
}
