package events

// Filter narrows which events a subscriber receives. An empty Filter
// matches every event.
//
// Scopes is an allow-list: if non-empty, only events whose Scope is
// in the list match. SessionID, when non-empty, requires an exact
// match (useful for tailing a single session's lifecycle events).
// SinceSeq is a lower bound on historical replay: the bus replays
// events with Seq > SinceSeq before switching to live delivery.
// Zero replays the full history. Subscribers that want live-only
// behavior fetch the current high-water mark and pass it here.
//
// LogicalAgentID is intentionally absent from Filter in v0.0.2 —
// logical-agent filtering is deferred until cross-session observability
// use cases emerge. Emitters that want per-agent filtering can embed
// the id in PayloadJSON today and post-filter client-side.
type Filter struct {
	Scopes    []Scope
	SessionID string
	SinceSeq  int64
}

func (f Filter) matches(e Event) bool {
	if len(f.Scopes) > 0 {
		found := false
		for _, s := range f.Scopes {
			if s == e.Scope {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.SessionID != "" && f.SessionID != e.SessionID {
		return false
	}
	return true
}
