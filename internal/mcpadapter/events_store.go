package mcpadapter

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// ToolCallEventFilter narrows which events are returned by ToolCallEventStore.Query.
// All fields are optional; an empty filter matches everything.
type ToolCallEventFilter struct {
	// ServerID filters by exact upstream server ID match.
	ServerID string

	// ToolName filters by prefix match on the tool name (e.g. "hadron_" matches all hadron tools).
	ToolName string

	// SessionID filters by exact tether session ID match.
	SessionID string

	// Limit caps the number of returned events. Zero means no cap.
	Limit int

	// SinceTimestamp excludes events that occurred before this time.
	SinceTimestamp time.Time

	// ErrorsOnly, when true, returns only events where OK is false.
	ErrorsOnly bool
}

// ToolCallEventStore is an in-memory ring buffer of ToolCallEnd events.
// It is safe for concurrent use. See ADR 0021 §Decision 2.
type ToolCallEventStore struct {
	mu       sync.RWMutex
	buf      []events.ToolCallEvent
	capacity int
	head     int // index of the oldest slot (circular)
	size     int // number of entries currently stored
}

// NewToolCallEventStore creates a store with the given ring buffer capacity.
// Panics when capacity <= 0.
func NewToolCallEventStore(capacity int) *ToolCallEventStore {
	if capacity <= 0 {
		panic("mcpadapter: ToolCallEventStore capacity must be > 0")
	}
	return &ToolCallEventStore{
		buf:      make([]events.ToolCallEvent, capacity),
		capacity: capacity,
	}
}

// Add appends a ToolCallEvent to the ring buffer, evicting the oldest entry
// when capacity is reached.
func (s *ToolCallEventStore) Add(ev events.ToolCallEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	slot := (s.head + s.size) % s.capacity
	s.buf[slot] = ev
	if s.size < s.capacity {
		s.size++
	} else {
		// Ring is full — advance head to drop the oldest entry.
		s.head = (s.head + 1) % s.capacity
	}
}

// Query returns a snapshot of events matching f, newest last (insertion order).
// The returned slice is a copy — safe to hold across concurrent Add calls.
func (s *ToolCallEventStore) Query(f ToolCallEventFilter) []events.ToolCallEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]events.ToolCallEvent, 0, s.size)
	for i := 0; i < s.size; i++ {
		idx := (s.head + i) % s.capacity
		ev := s.buf[idx]

		if f.ErrorsOnly && ev.OK {
			continue
		}
		if f.ServerID != "" && ev.Server != f.ServerID {
			continue
		}
		if f.ToolName != "" && !strings.HasPrefix(ev.ToolName, f.ToolName) {
			continue
		}
		if f.SessionID != "" && ev.SessionID != f.SessionID {
			continue
		}
		if !f.SinceTimestamp.IsZero() && !ev.Timestamp.After(f.SinceTimestamp) {
			continue
		}
		out = append(out, ev)
	}

	// Apply limit (take from the newest end).
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out
}

// Subscribe starts a background goroutine that follows EventTypeToolCallEnd
// events on bus and appends them to the store. It starts at the live head (the
// ring only needs recent calls; replaying a large history through it was the
// cost, not a feature), resumes from its cursor if the bus evicts it, and never
// sees stream.gap frames. The goroutine stops when ctx is done.
func (s *ToolCallEventStore) Subscribe(ctx context.Context, bus events.Bus) {
	events.FollowAsync(ctx, bus, events.Filter{Kinds: []string{events.EventTypeToolCallEnd}}, events.FollowOptions{LiveOnly: true, Name: "mcp-proxy: tool call event store"}, func(ev events.Event) {
		var tce events.ToolCallEvent
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &tce); err != nil {
			slog.Warn("mcp-proxy: failed to unmarshal ToolCallEvent from bus", "err", err)
			return
		}
		s.Add(tce)
	})
}
