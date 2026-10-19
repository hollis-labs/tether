package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// Source acceptance uses the service's production bus and the real session /
// registry resolver. This is fixture evidence, not owner live sampling.
func TestEventStageManagedAndUnmanagedAcceptance(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.EventStamp.SetProcess(eventstamp.Process{EnvironmentID: "fixture-env", Authority: "hub", Host: "fixture-host"})
	row := store.SessionRow{ID: "managed", State: "running", LogicalAgentID: "fixture-agent", ProjectID: "PRJ-1", LaunchID: "launch-1", WorkerID: "worker-1", TaskID: sql.NullString{String: "CW-20261016-0139", Valid: true}, WorkstreamID: sql.NullString{String: "WS-1", Valid: true}}
	if err := s.Store.CreateSession(row, nil); err != nil {
		t.Fatal(err)
	}
	agent := registry.LogicalAgentBindingTarget(row.LogicalAgentID)
	if _, err := s.Registry.LeaseBinding(context.Background(), agent, row.ID, "fixture-host", "attempt", nil, registry.VisibilityPrivateLocal, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		p       identity.Principal
		managed bool
	}{
		{"verified managed", identity.Principal{ID: "msg://session/local/managed", Kind: "session", SessionID: "managed", Verified: true}, true},
		{"verified unmanaged", identity.Principal{ID: "msg://session/local/unknown", Kind: "session", SessionID: "unknown", Verified: true}, false},
		{"unverified existing claim", identity.Principal{ID: "msg://session/local/managed", Kind: "session", SessionID: "managed"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := identity.WithPrincipal(context.Background(), tc.p)
			snap := api.ResolveCallerContext(ctx, s.Store, s.Registry)
			ctx = callcontext.WithSnapshot(ctx, snap)
			ch, stop, err := s.Bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			event := events.CanonicalToolCall(events.ToolCallEvent{ToolName: "read", SessionID: tc.p.SessionID, Attribution: snap}, true)
			if err := s.Bus.Publish(ctx, event); err != nil {
				t.Fatal(err)
			}
			var live events.Event
			select {
			case live = <-ch:
			case <-time.After(time.Second):
				t.Fatal("no live event")
			}
			stored, err := s.Store.EventsSince(live.Seq - 1)
			if err != nil || len(stored) == 0 {
				t.Fatalf("no stored event: %v", err)
			}
			for _, got := range []events.Event{live, stored[0]} {
				d := got.Envelope.Dims
				if d.EnvironmentID != "fixture-env" || d.Host != "fixture-host" || d.Authority != "hub" {
					t.Fatalf("process dimensions=%+v", d)
				}
				if tc.managed {
					if d.ProjectID != "PRJ-1" || d.TaskID != "CW-20261016-0139" || d.WorkstreamID != "WS-1" || d.WorkerID != "worker-1" || d.AgentURN != agent || d.LaunchID != "launch-1" || d.LogicalAgentID != "fixture-agent" {
						t.Fatalf("managed dimensions=%+v", d)
					}
					if got.Envelope.Actor.URN != agent || got.Envelope.Actor.Kind != "agent" || got.Envelope.Actor.Verified != "token" {
						t.Fatalf("managed actor=%+v", got.Envelope.Actor)
					}
				} else if d.ProjectID != "" || d.TaskID != "" || d.WorkstreamID != "" || d.WorkerID != "" || d.AgentURN != "" || got.Envelope.Actor.URN != "" {
					t.Fatalf("unmanaged attribution: %+v %+v", d, got.Envelope.Actor)
				}
			}
			if live.EventID == "" || live.EventID != stored[0].EventID {
				t.Fatal("replay changed identity")
			}
		})
	}
}

func TestNewServiceSelectsEventStageModeAtStartup(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("version: test\ndaemon:\n  event_stage_mode: "+mode+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			svc, err := New(root)
			if mode == "invalid" {
				if err == nil {
					svc.Close()
					t.Fatal("invalid startup mode accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			if err := svc.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeDaemon, Kind: events.KindSessionUsage, PayloadJSON: `{"input_tokens":1,"output_tokens":2,"turns":1}`}); err != nil {
				t.Fatal(err)
			}
			rows, err := svc.Store.EventsSince(0)
			if err != nil || len(rows) == 0 {
				t.Fatal("no event", err)
			}
			if rows[len(rows)-1].Envelope.Class.Sanitized != (mode == "enforce") {
				t.Fatal("startup mode was ignored")
			}
			if svc.EventPipeline.Counts()[events.KindSessionUsage].Events != 1 {
				t.Fatal("production chain counters missing")
			}
		})
	}
}
