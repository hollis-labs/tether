// Package turnrouting attaches staged turn output to its configured channel.
// The stage flag is the durable retry queue; event delivery only accelerates it.
package turnrouting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

var actor = gomsg.Address{Kind: gomsg.KindService, Authority: "local", ID: "turn-router"}

type retry struct {
	next   time.Time
	delay  time.Duration
	reason string
}

type Router struct {
	now        func() time.Time
	ticks      <-chan time.Time
	afterScan  func()
	db         *store.Store
	bus        events.Bus
	channels   *channels.Service
	running    atomic.Bool
	cancel     context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
	scanCursor string
	scanSeen   map[string]bool
	retries    map[string]retry // owned by worker
}

func New(db *store.Store, bus events.Bus, channelService *channels.Service) *Router {
	return &Router{db: db, bus: bus, channels: channelService, retries: make(map[string]retry)}
}

func (r *Router) subscribe(ctx context.Context) (<-chan events.Event, func(), error) {
	seq, err := r.db.MaxEventSeqContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	return r.bus.Subscribe(ctx, events.Filter{SinceSeq: seq, Kinds: []string{events.KindSessionTurnOutput}})
}

// Start installs the live sink before scanning pending stages. It is called once
// by daemon composition, before accepting launches.
func (r *Router) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	live, unsubscribe, err := r.subscribe(ctx)
	if err != nil {
		cancel()
		return err
	}
	r.cancel = cancel
	r.done = make(chan struct{})
	r.running.Store(true)
	go r.run(ctx, live, unsubscribe)
	return nil
}

func (r *Router) Running() bool { return r != nil && r.running.Load() }

func (r *Router) Close() {
	if r == nil || r.cancel == nil {
		return
	}
	r.closeOnce.Do(r.cancel)
	<-r.done
}

func (r *Router) run(ctx context.Context, live <-chan events.Event, unsubscribe func()) {
	defer close(r.done)
	defer r.running.Store(false)
	defer func() { unsubscribe() }()
	ticks := r.ticks
	if ticks == nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	r.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-live:
			if !ok {
				live = nil
				continue
			}
			var output events.TurnOutputEvent
			if json.Unmarshal([]byte(event.PayloadJSON), &output) == nil && output.MessageID != "" && output.SessionID == event.SessionID {
				r.attempt(ctx, store.PendingTurnOutput{MessageID: output.MessageID, SessionID: output.SessionID})
			}
		case <-ticks:
			if live == nil {
				var err error
				live, unsubscribe, err = r.subscribe(ctx)
				if err != nil {
					log.Printf("turn router: resubscribe: %v", err)
					live = nil
					unsubscribe = func() {}
				}
			}
			r.sweep(ctx)
		}
	}
}

func (r *Router) scan(ctx context.Context) {
	if r.scanSeen == nil {
		r.scanSeen = make(map[string]bool)
	}
	for pageNumber := 0; pageNumber < 8 && ctx.Err() == nil; pageNumber++ {
		page, err := r.db.PendingTurnOutputs(ctx, r.scanCursor, 128)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("turn router: pending scan: %v", err)
			}
			return
		}
		for _, item := range page {
			r.scanSeen[item.MessageID] = true
			r.attempt(ctx, item)
			r.scanCursor = item.MessageID
		}
		if len(page) < 128 {
			for id := range r.retries {
				if !r.scanSeen[id] {
					delete(r.retries, id)
				}
			}
			r.scanCursor = ""
			r.scanSeen = nil
			return
		}
	}
}

func (r *Router) attempt(ctx context.Context, item store.PendingTurnOutput) {
	if state, ok := r.retries[item.MessageID]; ok && r.clock().Before(state.next) {
		return
	}
	if err := r.attach(ctx, item); err != nil {
		if ctx.Err() != nil {
			return
		}
		previous := r.retries[item.MessageID]
		delay := previous.delay * 2
		if delay == 0 {
			delay = time.Second
		}
		if delay > time.Minute {
			delay = time.Minute
		}
		reason := err.Error()
		if previous.reason != reason {
			log.Printf("turn router: message %q: %v", item.MessageID, err)
		}
		r.retries[item.MessageID] = retry{next: r.clock().Add(delay), delay: delay, reason: reason}
	} else {
		delete(r.retries, item.MessageID)
	}
}

func (r *Router) attach(ctx context.Context, item store.PendingTurnOutput) error {
	route, err := r.db.SessionRoute(ctx, item.SessionID)
	if err != nil {
		return err
	}
	if route == nil {
		return fmt.Errorf("session has no route")
	}
	env, err := r.db.StagedTurnOutput(ctx, item.MessageID)
	if errors.Is(err, gomsg.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(route.Kinds, env.Metadata["kind"]) {
		return fmt.Errorf("output kind is not selected")
	}
	sender := gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: item.SessionID}
	ctx = identity.WithPrincipal(ctx, identity.Principal{ID: sender.URN(), Kind: "session", SessionID: item.SessionID, Addresses: []string{sender.URN()}, CreatedBy: actor.URN()})
	_, err = r.channels.AttachExisting(ctx, channels.ExistingMessage{MessageID: item.MessageID, Channel: route.Channel, SessionID: item.SessionID, Actor: actor})
	return err
}

func (r *Router) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
func (r *Router) sweep(ctx context.Context) {
	r.scan(ctx)
	if r.afterScan != nil {
		r.afterScan()
	}
}
