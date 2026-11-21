// Package agentlive builds the tether_agents_live view: every live agent, both
// Tether-launched sessions and kit-booted seats, with host, busy/idle state,
// last activity and a staleness flag derived from presence evidence.
//
// The package is pure: Service gathers the inputs and Build merges them, so the
// merge, freshness and filter rules are testable without a daemon.
package agentlive

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	KindSession = "tether_session"
	KindKitSeat = "kit_seat"

	// State is the consumer-facing presence: awake (a busy/idle observation),
	// sleeping (an explicit paused/offline declaration) or unknown. It is never
	// inferred from lifecycle or registration timestamps.
	StateAwake    = "awake"
	StateSleeping = "sleeping"
	StateUnknown  = "unknown"
	// StateStale is a kit seat not re-registered within its stale threshold.
	StateStale = "stale"

	// Raw busy/idle observations.
	RawBusy = "busy"
	RawIdle = "idle"

	DefaultStaleAfter = 10 * time.Minute
	MinStaleAfter     = time.Second
	MaxStaleAfter     = 24 * time.Hour
	// Kit-seat heartbeat thresholds: awake within DefaultSeatAwakeAfter of the
	// last registration, sleeping until DefaultSeatStaleAfter, stale beyond.
	DefaultSeatAwakeAfter = 10 * time.Minute
	DefaultSeatStaleAfter = 2 * time.Hour
	DefaultLimit          = 200
	MaxLimit              = 1000
)

// Query is the validated filter set.
type Query struct {
	Host         string
	Kind         string
	IncludeStale bool
	StaleAfter   time.Duration
	Limit        int
	// Offset is decoded from the opaque cursor.
	Offset int
	// SeatAwakeAfter and SeatStaleAfter are the kit-seat heartbeat thresholds;
	// zero means the defaults.
	SeatAwakeAfter time.Duration
	SeatStaleAfter time.Duration
	// staleSet records an explicit stale_after_seconds.
	staleSet bool
}

func (q Query) seatThresholds() (awake, stale time.Duration) {
	awake, stale = q.SeatAwakeAfter, q.SeatStaleAfter
	if awake <= 0 {
		awake = DefaultSeatAwakeAfter
	}
	if stale <= 0 {
		stale = DefaultSeatStaleAfter
	}
	if stale < awake {
		stale = awake
	}
	return awake, stale
}

// ParseQuery validates raw filter values. staleSeconds 0 means the default;
// anything outside 1..86400 seconds is an error, never clamped.
func ParseQuery(host, kind string, includeStale bool, staleSeconds, limit int, cursor string) (Query, error) {
	q := Query{Host: strings.TrimSpace(host), Kind: strings.TrimSpace(kind), IncludeStale: includeStale, StaleAfter: DefaultStaleAfter, Limit: DefaultLimit}
	if q.Kind != "" && q.Kind != KindSession && q.Kind != KindKitSeat {
		return q, errors.New("invalid kind: want tether_session or kit_seat")
	}
	if staleSeconds < 0 || time.Duration(staleSeconds)*time.Second > MaxStaleAfter {
		return q, errors.New("invalid stale_after_seconds: want 1..86400")
	}
	if staleSeconds > 0 {
		q.StaleAfter = time.Duration(staleSeconds) * time.Second
		q.staleSet = true
	}
	if limit < 0 || limit > MaxLimit {
		return q, errors.New("invalid limit: want 1..1000")
	}
	if limit > 0 {
		q.Limit = limit
	}
	if cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(cursor, "o"))
		if err != nil || !strings.HasPrefix(cursor, "o") || n < 0 {
			return q, errors.New("invalid cursor")
		}
		q.Offset = n
	}
	return q, nil
}

// AgentRef identifies the agent. Ref is the seat's declared work reference
// (for example task:<id>) and is omitted when none is declared.
type AgentRef struct {
	URN  string `json:"urn"`
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
}

// Row is one live agent.
type Row struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Agent AgentRef `json:"agent"`
	// Host is the machine name; omitted when unknown.
	Host string `json:"host,omitempty"`
	// State is awake, sleeping or unknown; see the State constants.
	State string `json:"state"`
	// RawState is the underlying observation: busy or idle when known,
	// otherwise the session state or the seat's declared status.
	RawState string `json:"raw_state,omitempty"`
	// LastActivityAt is null when no activity was observed.
	LastActivityAt     *time.Time `json:"last_activity_at"`
	LastActivitySource string     `json:"last_activity_source,omitempty"`
	// LastActivity is a one-line summary, present only when known.
	LastActivity string `json:"last_activity,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	WorkstreamID string `json:"workstream_id,omitempty"`
	// PresenceAt is the newest presence evidence; null means none was seen.
	PresenceAt     *time.Time `json:"presence_at"`
	PresenceSource string     `json:"presence_source,omitempty"`
	Stale          bool       `json:"stale"`
}

// Response is the tool/HTTP envelope.
type Response struct {
	Agents            []Row     `json:"agents"`
	Count             int       `json:"count"`
	Limit             int       `json:"limit"`
	Truncated         bool      `json:"truncated"`
	NextCursor        string    `json:"next_cursor,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
	StaleAfterSeconds int       `json:"stale_after_seconds"`
}

// Evidence is one dated sign that an agent is present.
type Evidence struct {
	At     time.Time
	Source string
}

func (e Evidence) newer(o Evidence) Evidence {
	if o.At.After(e.At) {
		return o
	}
	return e
}

// Declared is a seat's declared status (tether_status_set), already checked
// for expiry by the caller.
type Declared struct {
	State     string
	Reason    string
	Ref       string
	UpdatedAt time.Time
}

// SessionInput is a running or detached Tether session.
type SessionInput struct {
	ID, AgentURN, Name, Host, RawState string
	// Live is the runtime's busy/idle observation: RawBusy, RawIdle or "".
	Live string
	// Alive is true when the runtime confirmed the session is reachable now.
	Alive         bool
	LastActivity  time.Time
	WorkstreamID  string
	TaskID, RunID string
	Declared      *Declared
}

// SeatInput is an external kit-seat registration.
type SeatInput struct {
	URN, Session, RunID, TaskID, WorkstreamID string
	Host, Name, Summary                       string
	LastRegisteredAt                          time.Time
	Declared                                  *Declared
}

// Inputs is everything Build merges. Presence is keyed by agent URN and holds
// the newest watcher/binding evidence for that URN.
type Inputs struct {
	Now      time.Time
	Sessions []SessionInput
	Seats    []SeatInput
	Presence map[string]Evidence
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func declaredState(d *Declared) (state, raw, summary string, at time.Time) {
	if d == nil {
		return StateUnknown, "", "", time.Time{}
	}
	switch d.State {
	case "working":
		state, raw = StateAwake, RawBusy
	case "available":
		state, raw = StateAwake, RawIdle
	case "paused", "offline":
		state, raw = StateSleeping, d.State
	default:
		state, raw = StateUnknown, d.State
	}
	return state, raw, d.Reason, d.UpdatedAt
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Build merges the inputs into filtered, ordered rows. A row is stale when its
// newest presence evidence is older than q.StaleAfter (exactly at the threshold
// is fresh) or when there is no evidence at all. Registration time counts as
// evidence only at the moment it was registered, so an old registration decays
// to stale; a connected mail watcher is evidence "now", so a quiet watcher does
// not.
func Build(in Inputs, q Query) Response {
	now := in.Now.UTC()
	var rows []Row
	seenURN := map[string]bool{}
	for _, s := range in.Sessions {
		ev := Evidence{}
		if s.Alive {
			ev = Evidence{At: now, Source: "session_runtime"}
		} else if !s.LastActivity.IsZero() {
			ev = Evidence{At: s.LastActivity, Source: "session_activity"}
		}
		ev = ev.newer(in.Presence[s.AgentURN])
		seenURN[s.AgentURN] = true
		r := Row{ID: "session:" + s.ID, Kind: KindSession, Agent: AgentRef{URN: s.AgentURN, Name: s.Name}, Host: s.Host,
			State: StateUnknown, RawState: s.RawState, SessionID: s.ID, RunID: s.RunID, TaskID: s.TaskID, WorkstreamID: s.WorkstreamID,
			LastActivityAt: timePtr(s.LastActivity)}
		if s.Declared != nil {
			r.Agent.Ref = s.Declared.Ref
		}
		if s.Live == RawBusy || s.Live == RawIdle {
			r.State, r.RawState = StateAwake, s.Live
		}
		if r.LastActivityAt != nil {
			r.LastActivitySource = "session_runtime"
		}
		if _, _, sum, _ := declaredState(s.Declared); sum != "" {
			r.LastActivity = oneLine(sum)
		}
		rows = append(rows, finish(r, ev, now, q))
	}
	for _, k := range in.Seats {
		seenURN[k.URN] = true
		ev := Evidence{At: k.LastRegisteredAt, Source: "registration"}.newer(in.Presence[k.URN])
		state, raw, sum, at := declaredState(k.Declared)
		awakeAfter, staleAfter := q.seatThresholds()
		// Newest evidence (registration, watcher or binding), the same the
		// stale flag uses, so state and stale agree.
		age := now.Sub(ev.At)
		regState := StateStale
		switch {
		case age <= awakeAfter:
			regState = StateAwake
		case age <= staleAfter:
			regState = StateSleeping
		}
		if k.Declared == nil {
			// No explicit declaration: the registration heartbeat decides.
			state = regState
		}
		r := Row{ID: "seat:" + k.URN + ":" + k.Session, Kind: KindKitSeat, Agent: AgentRef{URN: k.URN, Name: k.Name}, Host: k.Host,
			State: state, RawState: raw, SessionID: k.Session, RunID: k.RunID, TaskID: k.TaskID, WorkstreamID: k.WorkstreamID,
			LastActivity: oneLine(k.Summary), LastActivityAt: timePtr(at)}
		if k.Declared != nil {
			r.Agent.Ref = k.Declared.Ref
		}
		if sum != "" {
			r.LastActivity = oneLine(sum)
		}
		if r.LastActivityAt != nil {
			r.LastActivitySource = "seat_status"
		} else if !k.LastRegisteredAt.IsZero() {
			r.LastActivityAt, r.LastActivitySource = timePtr(k.LastRegisteredAt), "registration"
		}
		if r.Agent.Name == "" {
			r.Agent.Name = k.URN[strings.LastIndex(k.URN, "/")+1:]
		}
		// A seat stays listed through its sleeping window even when the query's
		// stale_after is shorter: it is stale once past the seat threshold.
		qs := q
		if !qs.staleSet || staleAfter > qs.StaleAfter {
			qs.StaleAfter = staleAfter
		}
		rows = append(rows, finish(r, ev, now, qs))
	}
	// Watcher/binding-only agents: presence with no session or registration.
	for urn, ev := range in.Presence {
		if seenURN[urn] {
			continue
		}
		r := Row{ID: "seat:" + urn, Kind: KindKitSeat, Agent: AgentRef{URN: urn, Name: urn[strings.LastIndex(urn, "/")+1:]}, State: StateUnknown}
		rows = append(rows, finish(r, ev, now, q))
	}
	out := rows[:0]
	for _, r := range rows {
		if q.Kind != "" && r.Kind != q.Kind {
			continue
		}
		if q.Host != "" && !strings.EqualFold(r.Host, q.Host) {
			continue
		}
		if r.Stale && !q.IncludeStale {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].PresenceAt, out[j].PresenceAt
		switch {
		case a == nil && b != nil:
			return false
		case a != nil && b == nil:
			return true
		case a != nil && !a.Equal(*b):
			return a.After(*b)
		}
		return out[i].ID < out[j].ID
	})
	resp := Response{ObservedAt: now, StaleAfterSeconds: int(q.StaleAfter / time.Second)}
	resp.Limit = q.Limit
	if q.Offset >= len(out) {
		out = nil
	} else {
		out = out[q.Offset:]
	}
	if len(out) > q.Limit {
		out, resp.Truncated = out[:q.Limit], true
		resp.NextCursor = "o" + strconv.Itoa(q.Offset+q.Limit)
	}
	if out == nil {
		out = []Row{}
	}
	resp.Agents, resp.Count = out, len(out)
	return resp
}

func finish(r Row, ev Evidence, now time.Time, q Query) Row {
	r.PresenceAt = timePtr(ev.At)
	if r.PresenceAt != nil {
		r.PresenceSource = ev.Source
	}
	r.Stale = ev.At.IsZero() || now.Sub(ev.At) > q.StaleAfter
	return r
}

// Tracker records which agent mailboxes have an open watch stream. A connected
// watcher is presence "now"; a closed one leaves its disconnect time.
type Tracker struct {
	mu    sync.Mutex
	now   func() time.Time
	seats map[string]*watchState
}

type watchState struct {
	active int
	last   time.Time
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{now: time.Now, seats: map[string]*watchState{}} }

// Watch marks urn as watched until the returned release is called.
func (t *Tracker) Watch(urn string) (release func()) {
	t.mu.Lock()
	st := t.seats[urn]
	if st == nil {
		st = &watchState{}
		t.seats[urn] = st
	}
	st.active++
	st.last = t.now()
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			st.active--
			st.last = t.now()
			t.mu.Unlock()
		})
	}
}

// Snapshot returns watcher evidence per URN at now. Entries whose last sign is
// older than keep are dropped so the map stays bounded.
func (t *Tracker) Snapshot(now time.Time, keep time.Duration) map[string]Evidence {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]Evidence{}
	for urn, st := range t.seats {
		switch {
		case st.active > 0:
			out[urn] = Evidence{At: now, Source: "watcher"}
		case now.Sub(st.last) > keep:
			delete(t.seats, urn)
		default:
			out[urn] = Evidence{At: st.last, Source: "watcher"}
		}
	}
	return out
}
