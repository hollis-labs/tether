package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// FollowBackoff bounds how fast Follow re-subscribes after the bus closed its
// channel. Variables so tests can shorten them.
var (
	FollowBackoffFloor   = 200 * time.Millisecond
	FollowBackoffCeiling = 10 * time.Second
)

// FollowOptions tune Follow.
type FollowOptions struct {
	// LiveOnly starts at the current head instead of replaying history. It needs
	// a bus that implements LiveSubscriber; any other bus replays from
	// Filter.SinceSeq as usual. After the first event, resumption always uses
	// the cursor.
	LiveOnly bool
	// Name labels log lines.
	Name string
	// Ready, when set, is called once, after the first subscription attempt (so
	// a caller can publish knowing a live-only follower is registered).
	Ready func()
}

// FollowAsync starts Follow on its own goroutine and returns once the first
// subscription is registered, so events published after it returns are seen
// even by a live-only follower.
func FollowAsync(ctx context.Context, bus Bus, f Filter, opts FollowOptions, handle func(Event)) {
	ready := make(chan struct{})
	opts.Ready = func() { close(ready) }
	go Follow(ctx, bus, f, opts, handle)
	select {
	case <-ready:
	case <-ctx.Done():
	}
}

// Follow delivers matching events to handle until ctx is done. It owns the
// subscription lifecycle: it tracks a cursor (the newest sequence it has seen,
// including the one a stream.gap frame carries), and when the bus closes the
// channel (the subscriber was evicted for lack of progress) it re-subscribes
// from the cursor with a backoff instead of silently ending. Gap frames are
// logged and never passed to handle. handle runs on Follow's goroutine; a slow
// handler is what causes overrun, which a paged bus recovers from the durable
// log.
func Follow(ctx context.Context, bus Bus, f Filter, opts FollowOptions, handle func(Event)) {
	cursor := f.SinceSeq
	liveFirst := opts.LiveOnly
	minBackoff, maxBackoff := FollowBackoffFloor, FollowBackoffCeiling // read once: the vars are for tests
	backoff := minBackoff
	name := opts.Name
	if name == "" {
		name = "events.Follow"
	}
	for ctx.Err() == nil {
		sub := f
		sub.SinceSeq = cursor
		var (
			ch     <-chan Event
			cancel func()
			err    error
		)
		if live, ok := bus.(LiveSubscriber); ok && liveFirst && cursor == f.SinceSeq {
			ch, cancel, err = live.SubscribeLive(ctx, sub)
		} else {
			ch, cancel, err = bus.Subscribe(ctx, sub)
		}
		liveFirst = false
		if opts.Ready != nil {
			opts.Ready()
			opts.Ready = nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn(name+": subscribe failed", "err", err, "retry_in", backoff)
		} else {
			progressed := follow(ctx, name, ch, &cursor, handle)
			cancel()
			if progressed {
				backoff = minBackoff
			}
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// follow drains one subscription. It reports whether any event arrived.
func follow(ctx context.Context, name string, ch <-chan Event, cursor *int64, handle func(Event)) (progressed bool) {
	for {
		select {
		case <-ctx.Done():
			return progressed
		case e, ok := <-ch:
			if !ok {
				return progressed
			}
			progressed = true
			if e.Seq > *cursor {
				*cursor = e.Seq
			}
			if e.Kind == KindStreamGap {
				logGap(name, e)
				continue
			}
			handle(e)
		}
	}
}

func logGap(name string, e Event) {
	var g GapPayload
	if json.Unmarshal([]byte(e.PayloadJSON), &g) != nil {
		slog.Warn(name+": stream gap", "payload", e.PayloadJSON)
		return
	}
	attrs := []any{"reason", g.Reason, "dropped", g.Dropped, "last_seq", g.LastSeq, "earliest_available", g.EarliestAvailable, "high_water", g.HighWater, "recovered", g.Recovered}
	if g.Reason == "trimmed" || g.Reason == "replay_error" || !g.Recovered && g.Reason == "overrun" {
		slog.Warn(name+": events were lost", attrs...)
		return
	}
	slog.Debug(name+": stream gap", attrs...)
}
