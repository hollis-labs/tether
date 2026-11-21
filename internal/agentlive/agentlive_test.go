package agentlive

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func q(t *testing.T, host, kind string, stale bool, secs int) Query {
	t.Helper()
	out, err := ParseQuery(host, kind, stale, secs, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseQueryValidation(t *testing.T) {
	if d := q(t, "", "", false, 0); d.StaleAfter != 10*time.Minute || d.Limit != DefaultLimit {
		t.Fatalf("defaults: %+v", d)
	}
	for _, c := range []struct {
		kind  string
		stale int
		limit int
	}{{"bogus", 0, 0}, {"", -1, 0}, {"", 86401, 0}, {"", 0, -1}, {"", 0, 1001}} {
		if _, err := ParseQuery("", c.kind, false, c.stale, c.limit, ""); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if o, err := ParseQuery("", "", false, 86400, 1000, ""); err != nil || o.StaleAfter != 24*time.Hour {
		t.Fatalf("upper bound: %+v %v", o, err)
	}
}

func TestSessionRows(t *testing.T) {
	in := Inputs{Now: t0, Sessions: []SessionInput{
		{ID: "s1", AgentURN: "msg://agent/agent-mux/a", Name: "a", Host: "box", RawState: "running", Live: RawBusy, Alive: true, LastActivity: t0.Add(-time.Minute), WorkstreamID: "workstream:X"},
		{ID: "s2", AgentURN: "msg://agent/agent-mux/b", Name: "b", Host: "box", RawState: "detached", LastActivity: t0.Add(-30 * time.Minute)},
		{ID: "s3", AgentURN: "msg://agent/agent-mux/c", Name: "c", RawState: "running"},
	}}
	got := Build(in, q(t, "", "", true, 0))
	if got.Count != 3 || got.StaleAfterSeconds != 600 {
		t.Fatalf("count/threshold: %+v", got)
	}
	by := map[string]Row{}
	for _, r := range got.Agents {
		by[r.SessionID] = r
	}
	if r := by["s1"]; r.Stale || r.State != StateAwake || r.RawState != RawBusy || r.PresenceSource != "session_runtime" || r.WorkstreamID != "workstream:X" || r.Kind != KindSession {
		t.Errorf("s1: %+v", r)
	}
	if r := by["s2"]; !r.Stale || r.State != StateUnknown || r.PresenceSource != "session_activity" {
		t.Errorf("s2 must be stale/unknown: %+v", r)
	}
	if r := by["s3"]; !r.Stale || r.PresenceAt != nil || r.LastActivityAt != nil {
		t.Errorf("s3 unknown must not look fresh: %+v", r)
	}
	if def := Build(in, q(t, "", "", false, 0)); def.Count != 1 || def.Agents[0].SessionID != "s1" {
		t.Errorf("stale hidden by default: %+v", def)
	}
}

func TestSeatFreshnessAndThreshold(t *testing.T) {
	urn := "msg://agent/agent-mux/seat"
	seat := func(age time.Duration) Inputs {
		return Inputs{Now: t0, Seats: []SeatInput{{URN: urn, Session: "k1", LastRegisteredAt: t0.Add(-age), Host: "h"}}}
	}
	// Boundary: exactly at the seat stale threshold (2h) is fresh, one
	// nanosecond past is stale; the sleeping window stays listed.
	if r := Build(seat(2*time.Hour), q(t, "", "", true, 0)).Agents[0]; r.Stale {
		t.Error("exactly at threshold must be fresh")
	}
	if r := Build(seat(2*time.Hour+time.Nanosecond), q(t, "", "", true, 0)).Agents[0]; !r.Stale {
		t.Error("past threshold must be stale")
	}
	// A longer stale_after than the seat threshold wins.
	if r := Build(seat(3*time.Hour), q(t, "", "", true, 4*3600)).Agents[0]; r.Stale {
		t.Error("custom 4h threshold")
	}
	// A very old registration is not perpetually fresh.
	if r := Build(seat(72*time.Hour), q(t, "", "", true, 0)).Agents[0]; !r.Stale || r.PresenceSource != "registration" {
		t.Errorf("old registration: %+v", r)
	}
	// A connected quiet watcher keeps an old registration fresh.
	in := seat(72 * time.Hour)
	in.Presence = map[string]Evidence{urn: {At: t0, Source: "watcher"}}
	if r := Build(in, q(t, "", "", false, 0)).Agents[0]; r.Stale || r.PresenceSource != "watcher" {
		t.Errorf("watcher: %+v", r)
	}
	// A disconnected watcher decays with its disconnect time.
	in.Presence = map[string]Evidence{urn: {At: t0.Add(-3 * time.Hour), Source: "watcher"}}
	if r := Build(in, q(t, "", "", true, 0)).Agents[0]; !r.Stale {
		t.Errorf("disconnected watcher: %+v", r)
	}
}

func TestSeatDeclaredStateAndPresenceOnlyRows(t *testing.T) {
	urn := "msg://agent/agent-mux/lead"
	in := Inputs{Now: t0,
		Seats: []SeatInput{
			{URN: urn, Session: "k1", LastRegisteredAt: t0, Declared: &Declared{State: "working", Ref: "task:T", Reason: "  doing\nthe thing ", UpdatedAt: t0.Add(-time.Minute)}},
			{URN: "msg://agent/agent-mux/blocked", Session: "k2", LastRegisteredAt: t0, Declared: &Declared{State: "blocked", UpdatedAt: t0}},
			{URN: "msg://agent/agent-mux/asleep", Session: "k4", LastRegisteredAt: t0, Declared: &Declared{State: "paused", UpdatedAt: t0}},
			{URN: "msg://agent/agent-mux/none", Session: "k3", LastRegisteredAt: t0},
		},
		Presence: map[string]Evidence{"msg://agent/agent-mux/ghost": {At: t0, Source: "watcher"}, urn: {At: t0, Source: "binding"}}}
	got := Build(in, q(t, "", KindKitSeat, false, 0))
	by := map[string]Row{}
	for _, r := range got.Agents {
		by[r.Agent.URN] = r
	}
	if r := by[urn]; r.State != StateAwake || r.RawState != RawBusy || r.LastActivity != "doing the thing" || r.LastActivityAt == nil || r.LastActivitySource != "seat_status" {
		t.Errorf("working seat: %+v", r)
	}
	if r := by["msg://agent/agent-mux/blocked"]; r.State != StateUnknown || r.RawState != "blocked" {
		t.Errorf("blocked is unknown, raw kept: %+v", r)
	}
	if r := by["msg://agent/agent-mux/asleep"]; r.State != StateSleeping || r.RawState != "paused" {
		t.Errorf("explicit pause is sleeping: %+v", r)
	}
	if r := by["msg://agent/agent-mux/none"]; r.State != StateAwake || r.LastActivityAt == nil || r.LastActivitySource != "registration" {
		t.Errorf("no declaration: %+v", r)
	}
	if r := by["msg://agent/agent-mux/ghost"]; r.Kind != KindKitSeat || r.SessionID != "" || r.Agent.Name != "ghost" || r.Stale {
		t.Errorf("watcher-only row: %+v", r)
	}
}

func TestMultipleSessionsDeduplicateAndFilters(t *testing.T) {
	urn := "msg://agent/agent-mux/dup"
	in := Inputs{Now: t0,
		Sessions: []SessionInput{
			{ID: "a", AgentURN: urn, Host: "H1", Alive: true},
			{ID: "b", AgentURN: urn, Host: "h2", Alive: true},
		},
		Seats:    []SeatInput{{URN: "msg://agent/agent-mux/k", Session: "k1", Host: "H1", LastRegisteredAt: t0}},
		Presence: map[string]Evidence{urn: {At: t0, Source: "watcher"}}}
	if got := Build(in, q(t, "", "", false, 0)); got.Count != 3 {
		t.Fatalf("two sessions + seat, watcher must not add a row for a known URN: %+v", got.Agents)
	}
	if got := Build(in, q(t, "h1", "", false, 0)); got.Count != 2 {
		t.Errorf("host filter is case-insensitive: %d", got.Count)
	}
	if got := Build(in, q(t, "h1", KindSession, false, 0)); got.Count != 1 || got.Agents[0].SessionID != "a" {
		t.Errorf("host+kind compose: %+v", got.Agents)
	}
	if got := Build(in, q(t, "nowhere", "", false, 0)); got.Count != 0 || got.Agents == nil {
		t.Errorf("empty result must be a non-nil list: %+v", got)
	}
	lim, _ := ParseQuery("", "", false, 0, 2, "")
	got := Build(in, lim)
	if got.Count != 2 || !got.Truncated || got.Limit != 2 || got.NextCursor != "o2" {
		t.Fatalf("limit/truncated: %+v", got)
	}
	next, err := ParseQuery("", "", false, 0, 2, got.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if p2 := Build(in, next); p2.Count != 1 || p2.Truncated || p2.NextCursor != "" || p2.Agents[0].ID == got.Agents[0].ID || p2.Agents[0].ID == got.Agents[1].ID {
		t.Errorf("second page: %+v", p2)
	}
	for _, bad := range []string{"zz", "o-1", "5", "oabc"} {
		if _, err := ParseQuery("", "", false, 0, 0, bad); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker()
	now := t0
	tr.now = func() time.Time { return now }
	rel := tr.Watch("u")
	rel2 := tr.Watch("u")
	now = t0.Add(time.Hour)
	if ev := tr.Snapshot(now, 24*time.Hour)["u"]; !ev.At.Equal(now) || ev.Source != "watcher" {
		t.Fatalf("active watcher is now: %+v", ev)
	}
	rel()
	if ev := tr.Snapshot(now, 24*time.Hour)["u"]; !ev.At.Equal(now) {
		t.Fatalf("one of two still open: %+v", ev)
	}
	rel2()
	rel2() // idempotent
	closedAt := now
	now = now.Add(5 * time.Minute)
	if ev := tr.Snapshot(now, 24*time.Hour)["u"]; !ev.At.Equal(closedAt) {
		t.Fatalf("closed watcher keeps its disconnect time: %+v", ev)
	}
	now = now.Add(48 * time.Hour)
	if _, ok := tr.Snapshot(now, 24*time.Hour)["u"]; ok {
		t.Fatal("old entry must be dropped")
	}
}

func TestKitSeatStateFromHeartbeat(t *testing.T) {
	urn := "msg://agent/agent-mux/hb"
	state := func(age time.Duration, mod func(*Query)) Row {
		qq := q(t, "", "", true, 0)
		if mod != nil {
			mod(&qq)
		}
		in := Inputs{Now: t0, Seats: []SeatInput{{URN: urn, Session: "k", Host: "sol", Name: "Heart Beat", LastRegisteredAt: t0.Add(-age)}}}
		return Build(in, qq).Agents[0]
	}
	cases := []struct {
		age   time.Duration
		want  string
		stale bool
	}{
		{0, StateAwake, false},
		{10 * time.Minute, StateAwake, false},
		{10*time.Minute + time.Nanosecond, StateSleeping, false},
		{2 * time.Hour, StateSleeping, false},
		{2*time.Hour + time.Nanosecond, StateStale, true},
	}
	for _, c := range cases {
		r := state(c.age, nil)
		if r.State != c.want || r.Stale != c.stale || r.Agent.Name != "Heart Beat" || r.Host != "sol" {
			t.Errorf("age %v: %+v", c.age, r)
		}
		if r.LastActivityAt == nil || !r.LastActivityAt.Equal(t0.Add(-c.age)) {
			t.Errorf("age %v last_activity_at: %+v", c.age, r.LastActivityAt)
		}
	}
	// Configurable thresholds.
	cfg := func(qq *Query) { qq.SeatAwakeAfter, qq.SeatStaleAfter = time.Minute, 5*time.Minute }
	if r := state(2*time.Minute, cfg); r.State != StateSleeping {
		t.Errorf("custom awake: %+v", r)
	}
	if r := state(6*time.Minute, cfg); r.State != StateStale || !r.Stale {
		t.Errorf("custom stale: %+v", r)
	}
	// Name falls back to the URN tail without a display name.
	in := Inputs{Now: t0, Seats: []SeatInput{{URN: urn, Session: "k", LastRegisteredAt: t0}}}
	if r := Build(in, q(t, "", "", true, 0)).Agents[0]; r.Agent.Name != "hb" || r.Host != "" {
		t.Errorf("fallback: %+v", r)
	}
}
