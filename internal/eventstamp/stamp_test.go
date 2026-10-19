package eventstamp

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"go.opentelemetry.io/otel/trace"
)

func stamp(ctx context.Context, t *testing.T, s *Stage, e events.Event) events.Event {
	t.Helper()
	if err := s.Stamp(ctx, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

var resolved = callcontext.Snapshot{Verified: true, SessionResolved: true, PrincipalKind: "session", PrincipalID: "msg://session/local/s1", SessionID: "s1",
	AgentURN: "msg://agent/fleet/one", WorkstreamID: "ws-1", TaskID: "CW-1", LaunchID: "tether-claude", ProjectID: "PRJ-1", LogicalAgentID: "la-1"}

func sessionCtx(snap callcontext.Snapshot, p identity.Principal) context.Context {
	return identity.WithPrincipal(callcontext.WithSnapshot(context.Background(), snap), p)
}

var sessionPrincipal = identity.Principal{Verified: true, ID: "msg://session/local/s1", Kind: "session", SessionID: "s1"}

func TestProcessDimensionsAreStampedOnlyOnceSet(t *testing.T) {
	s := New()
	got := stamp(context.Background(), t, s, events.Event{Scope: events.ScopeDaemon, Kind: "k"})
	if got.Envelope.Dims.EnvironmentID != "" || got.Envelope.Dims.Host != "" {
		t.Fatalf("stamped before the process is known: %+v", got.Envelope.Dims)
	}
	s.SetProcess(Process{EnvironmentID: "env-1", Authority: "hub", Host: "agent-os"})
	got = stamp(context.Background(), t, s, events.Event{Scope: events.ScopeDaemon, Kind: "k"})
	if d := got.Envelope.Dims; d.EnvironmentID != "env-1" || d.Authority != "hub" || d.Host != "agent-os" {
		t.Fatalf("process dims: %+v", d)
	}
	// Nothing session-scoped comes from the process.
	if d := got.Envelope.Dims; d.AgentURN != "" || d.WorkstreamID != "" || d.ProjectID != "" || d.WorkerID != "" {
		t.Fatalf("session dims invented: %+v", d)
	}
}

func TestResolvedSessionStampsItsOwnEventsOnly(t *testing.T) {
	s := New()
	ctx := sessionCtx(resolved, sessionPrincipal)
	got := stamp(ctx, t, s, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "tool.call"})
	want := events.ScopeDims{AgentURN: "msg://agent/fleet/one", WorkstreamID: "ws-1", TaskID: "CW-1", LaunchID: "tether-claude", ProjectID: "PRJ-1", LogicalAgentID: "la-1"}
	if got.Envelope.Dims != want {
		t.Fatalf("dims %+v, want %+v", got.Envelope.Dims, want)
	}
	// The caller's snapshot says nothing about another session's event, or
	// about an event with no session.
	for _, other := range []string{"s2", ""} {
		got = stamp(ctx, t, s, events.Event{Scope: events.ScopeSession, SessionID: other, Kind: "k"})
		if got.Envelope.Dims != (events.ScopeDims{}) {
			t.Fatalf("session %q event got the caller's dims: %+v", other, got.Envelope.Dims)
		}
	}
}

func TestUnresolvedOrUnmanagedCallerStampsNothing(t *testing.T) {
	s := New()
	unresolved := resolved
	unresolved.SessionResolved = false
	for name, ctx := range map[string]context.Context{
		"snapshot not resolved": sessionCtx(unresolved, sessionPrincipal),
		"no snapshot":           identity.WithPrincipal(context.Background(), sessionPrincipal),
		"claimed session only":  callcontext.WithClaimedSession(context.Background(), "s1"),
		"nothing":               context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			got := stamp(ctx, t, s, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "k"})
			if got.Envelope.Dims != (events.ScopeDims{}) || actorSet(got.Envelope.Actor) {
				t.Fatalf("stamped without proof: %+v", got.Envelope)
			}
		})
	}
}

func TestActorFollowsVerifiedFactsNotKind(t *testing.T) {
	s := New()
	operator := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}
	narrow := operator
	narrow.TokenID = "jti-1" // operator-minted narrow token: not the operator
	unverified := operator
	unverified.Verified = false
	bound := identity.Principal{Verified: true, ID: "msg://session/local/s1", Kind: "agent", SessionID: "s1", TokenID: "jti-2"}
	forgedBound := bound
	forgedBound.TokenID = "" // Kind agent alone is not a bound token
	foreignSession := sessionPrincipal
	foreignSession.SessionID = "s9"
	for name, tc := range map[string]struct {
		ctx  context.Context
		want events.Actor
	}{
		"operator":                    {identity.WithPrincipal(context.Background(), operator), events.Actor{Kind: "user", URN: identity.OperatorID, Verified: "token"}},
		"operator-minted token":       {identity.WithPrincipal(context.Background(), narrow), events.Actor{Kind: "user", URN: identity.OperatorID, Verified: "token"}},
		"device (no mesh kind yet)":   {identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "msg://device/laptop-1", Kind: "device"}), events.Actor{}},
		"service principal":           {identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "msg://service/local/member", Kind: "service"}), events.Actor{Kind: "service", URN: "msg://service/local/member", Verified: "token"}},
		"user principal":              {identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "msg://user/local/alice", Kind: "interactive"}), events.Actor{Kind: "user", URN: "msg://user/local/alice", Verified: "token"}},
		"agent principal":             {identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "msg://agent/fleet/two", Kind: "agent", TokenID: "jti-9"}), events.Actor{Kind: "agent", URN: "msg://agent/fleet/two", Verified: "token"}},
		"non-principal subject":       {identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "urn:something:else", Kind: "service"}), events.Actor{}},
		"unverified operator":         {identity.WithPrincipal(context.Background(), unverified), events.Actor{}},
		"session credential":          {sessionCtx(resolved, sessionPrincipal), events.Actor{Kind: "agent", URN: "msg://agent/fleet/one", Verified: "token"}},
		"issued bound token":          {sessionCtx(resolved, bound), events.Actor{Kind: "agent", URN: "msg://agent/fleet/one", Verified: "token"}},
		"agent kind alone":            {sessionCtx(resolved, forgedBound), events.Actor{}},
		"snapshot of another session": {sessionCtx(resolved, foreignSession), events.Actor{}},
	} {
		t.Run(name, func(t *testing.T) {
			got := stamp(tc.ctx, t, s, events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "k"})
			if !sameActor(got.Envelope.Actor, tc.want) {
				t.Fatalf("actor %+v, want %+v", got.Envelope.Actor, tc.want)
			}
		})
	}
	noAgent := resolved
	noAgent.AgentURN = ""
	if got := stamp(sessionCtx(noAgent, sessionPrincipal), t, s, events.Event{SessionID: "s1"}); actorSet(got.Envelope.Actor) {
		t.Fatalf("actor invented without a confirmed agent address: %+v", got.Envelope.Actor)
	}
}

func TestProducerValuesAreNeverOverwritten(t *testing.T) {
	s := New()
	s.SetProcess(Process{EnvironmentID: "stage-env", Host: "stage-host"})
	e := events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "k"}
	e.Envelope.Dims = events.ScopeDims{EnvironmentID: "producer-env", ProjectID: "producer-project"}
	e.Envelope.Actor = events.Actor{Kind: "service", URN: "msg://service/producer/x", Verified: "claimed"}
	e.Envelope.Corr.TraceID = "producer-trace"
	got := stamp(sessionCtx(resolved, sessionPrincipal), t, s, e)
	if got.Envelope.Dims.EnvironmentID != "producer-env" || got.Envelope.Dims.ProjectID != "producer-project" || got.Envelope.Actor.URN != "msg://service/producer/x" || got.Envelope.Corr.TraceID != "producer-trace" {
		t.Fatalf("producer values overwritten: %+v", got.Envelope)
	}
	// What the producer left empty is still filled.
	if got.Envelope.Dims.Host != "stage-host" || got.Envelope.Dims.WorkstreamID != "ws-1" {
		t.Fatalf("empty fields not filled: %+v", got.Envelope.Dims)
	}
}

func TestCorrelationComesFromTheTracerOnly(t *testing.T) {
	s := New()
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}))
	got := stamp(ctx, t, s, events.Event{Scope: events.ScopeDaemon, Kind: "k"})
	if got.Envelope.Corr.TraceID != "0102030405060708090a0b0c0d0e0f10" || got.Envelope.Corr.SpanID != "0102030405060708" {
		t.Fatalf("corr: %+v", got.Envelope.Corr)
	}
	got = stamp(context.Background(), t, s, events.Event{Scope: events.ScopeDaemon, Kind: "k"})
	if got.Envelope.Corr != (events.Corr{}) {
		t.Fatalf("corr invented without a span: %+v", got.Envelope.Corr)
	}
}

func TestStageIsTxSafeAndNeverFails(t *testing.T) {
	var s events.Stage = New()
	safe, ok := s.(events.TxSafe)
	if !ok || !safe.TxSafe() {
		t.Fatal("stamp stage must be safe inside a caller's transaction: it reads no database")
	}
}

func TestNoTokenMaterialInTheEnvelope(t *testing.T) {
	s := New()
	p := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Display: "secret-display", Scopes: []string{"*"}, Addresses: []string{"msg://x/y/z"}, CreatedBy: "creator", Issuer: "iss", Audience: "aud", Parent: "parent"}
	got := stamp(identity.WithPrincipal(context.Background(), p), t, s, events.Event{Scope: events.ScopeDaemon, Kind: "k"})
	if got.Envelope.Actor.OnBehalfOf != "" || got.Envelope.Actor.URN != identity.OperatorID {
		t.Fatalf("actor: %+v", got.Envelope.Actor)
	}
}

// End to end through the real bus and SQLite log: what the stage stamps is what
// the events table stores, and an unresolved caller leaves the columns NULL.
func TestStampedValuesReachTheStoredRow(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New()
	s.SetProcess(Process{EnvironmentID: "env-1", Authority: "hub", Host: "agent-os"})
	bus := events.NewBus(events.BusOptions{Persister: db, Stages: []events.Stage{s}})
	if err := bus.Publish(sessionCtx(resolved, sessionPrincipal), events.Event{Scope: events.ScopeSession, SessionID: "s1", Kind: "tool.call", PayloadJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: "daemon.tick", PayloadJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	type row struct{ env, host, agent, project, task, actorKind, actorURN, verified sql.NullString }
	read := func(kind string) row {
		var r row
		err := db.DB().QueryRow(`SELECT scope_environment_id, scope_host, scope_agent_urn, scope_project_id, scope_task_id, actor_kind, actor_urn, actor_verified FROM events WHERE kind=?`, kind).
			Scan(&r.env, &r.host, &r.agent, &r.project, &r.task, &r.actorKind, &r.actorURN, &r.verified)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	tool := read("tool.call")
	if tool.env.String != "env-1" || tool.host.String != "agent-os" || tool.agent.String != "msg://agent/fleet/one" || tool.project.String != "PRJ-1" || tool.task.String != "CW-1" ||
		tool.actorKind.String != "agent" || tool.actorURN.String != "msg://agent/fleet/one" || tool.verified.String != "token" {
		t.Fatalf("stored tool.call: %+v", tool)
	}
	tick := read("daemon.tick")
	if tick.env.String != "env-1" || tick.agent.Valid || tick.project.Valid || tick.task.Valid || tick.actorKind.Valid || tick.actorURN.Valid || tick.verified.Valid {
		t.Fatalf("stored daemon event carries more than the process: %+v", tick)
	}
}

// sameActor compares the fields the stamp stage sets, so the test survives
// events.Actor gaining non-comparable fields.
func sameActor(a, b events.Actor) bool {
	return a.Kind == b.Kind && a.URN == b.URN && a.Verified == b.Verified && a.OnBehalfOf == b.OnBehalfOf
}

func TestCanonicalToolCorrelationUsesOnlyResolvedSession(t *testing.T) {
	for _, verified := range []bool{true, false} {
		snapshot := resolved
		snapshot.SessionResolved = verified
		for _, finished := range []bool{false, true} {
			e := events.CanonicalToolCall(events.ToolCallEvent{SessionID: "s1", ToolName: "read", Attribution: snapshot}, finished)
			got := stamp(sessionCtx(snapshot, sessionPrincipal), t, New(), e)
			if verified {
				if d := got.Envelope.Dims; d.TaskID != "CW-1" || d.ProjectID != "PRJ-1" || d.WorkstreamID != "ws-1" {
					t.Fatalf("resolved correlation=%+v", d)
				}
			} else if d := got.Envelope.Dims; d.TaskID != "" || d.ProjectID != "" || d.WorkstreamID != "" {
				t.Fatalf("claimed correlation=%+v", d)
			}
		}
	}
}
