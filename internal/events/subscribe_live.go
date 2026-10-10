package events

import "context"

// LiveSubscriber registers before the caller reads its bounded durable tail.
// Notifications may be coalesced/dropped; the durable store remains authoritative.
type LiveSubscriber interface {
	SubscribeLive(context.Context, Filter) (<-chan Event, func(), error)
}

func (b *memBus) SubscribeLive(ctx context.Context, f Filter) (<-chan Event, func(), error) {
	if err := b.lock(ctx); err != nil {
		return nil, nil, err
	}
	b.nextID++
	s := &subscriber{id: b.nextID, filter: f, live: make(chan Event, b.subBuffer), out: make(chan Event, b.subBuffer), done: make(chan struct{}), maxConsec: b.maxConsecDrops}
	b.subs[s.id] = s
	b.unlock()
	go b.runSubscriber(s)
	return s.out, func() { b.evict(s) }, nil
}
