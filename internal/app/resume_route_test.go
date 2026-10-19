package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestResumePreservesPersistedRouteOverrideAndAbsentOptIn(t *testing.T) {
	for _, routed := range []bool{true, false} {
		name := "absent"
		if routed {
			name = "override"
		}
		t.Run(name, func(t *testing.T) {
			rig := newCodexRig(t)
			// Recovery starts its subprocess control turn asynchronously. Hold
			// it until cleanup to exercise teardown while the producer is late.
			releaseInput, inputDone := make(chan struct{}), make(chan error, 1)
			baseFactory := rig.svc.factories["codex-cli"]
			var resumedRuntimeStarted bool
			rig.svc.factories["codex-cli"] = func(plan *launch.Plan) (agentsessions.Runtime, error) {
				rt, err := baseFactory(plan)
				if err != nil || plan.ResumeSourceSessionID == "" {
					return rt, err
				}
				return routeResumeRuntime{Runtime: rt, release: releaseInput, done: inputDone, started: &resumedRuntimeStarted}, nil
			}
			request := CreateSessionInput{LaunchID: "codex-launch"}
			if routed {
				request.Route = &launchprofile.Route{Channel: "override-channel", Kinds: []string{"question", "failure"}}
			}
			parent, err := rig.svc.CreateSessionWithInput(request)
			if err != nil {
				t.Fatal(err)
			}
			owned := []string{parent.SessionID}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if resumedRuntimeStarted {
					close(releaseInput)
					select {
					case err := <-inputDone:
						if err != nil {
							t.Errorf("fixture recovery input failed before teardown: %v", err)
						}
					case <-ctx.Done():
						t.Error("fixture recovery input did not finish before teardown")
						return
					}
				}
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				for _, id := range owned {
					if _, managed := rig.svc.Manager.Get(id); managed {
						_ = rig.svc.Manager.Stop(ctx, id)
						_, _ = rig.svc.Manager.WaitSession(ctx, id)
					}
					// Manager exit precedes the output finalizer's flush. Its
					// removal marks quiescence before the rig closes its DB and
					// removes directories that the producer may still be writing.
					for {
						if _, pending := rig.svc.turnOutputs.Load(id); !pending {
							break
						}
						select {
						case <-tick.C:
						case <-ctx.Done():
							t.Error("fixture output finalization did not finish before teardown")
							return
						}
					}
				}
				rig.svc.stopOutputRetries()
			})
			if _, err := rig.svc.LaunchSession(parent.SessionID); err != nil {
				t.Fatal(err)
			}
			if err := rig.svc.StopSession(parent.SessionID); err != nil {
				t.Fatal(err)
			}
			rig.ended(parent.SessionID)
			// Change catalog intent after the parent exists. The persisted parent wins.
			catalogLaunch := rig.svc.Catalog.Launches["codex-launch"]
			catalogLaunch.Route = &launchprofile.Route{Channel: "invalid catalog channel", Kinds: []string{"unknown"}}
			rig.svc.Catalog.Launches["codex-launch"] = catalogLaunch
			rig.checkpoint("resume-route", parent.SessionID, "")
			resumed, err := rig.svc.ResumeLogicalAgent("agent", api.ResumeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			owned = append(owned, resumed.SessionID)
			route, err := rig.svc.Store.SessionRoute(context.Background(), resumed.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !routed {
				if route != nil {
					t.Fatalf("resume introduced routing: %+v", route)
				}
			} else if route == nil || route.Channel != "override-channel" || !slices.Equal(route.Kinds, request.Route.Kinds) {
				t.Fatalf("resume lost persisted route: %+v", route)
			}
		})
	}
}

// This fixture is subprocess-only: no optional RPC/interrupt interfaces are
// replaced. Typed output callbacks still go through the real runtime.
type routeResumeRuntime struct {
	agentsessions.Runtime
	release <-chan struct{}
	done    chan error
	started *bool
}

func (r routeResumeRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	session, err := r.Runtime.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	*r.started = true
	return routeResumeSession{Session: session, release: r.release, done: r.done}, nil
}

type routeResumeSession struct {
	agentsessions.Session
	release <-chan struct{}
	done    chan error
}

func (s routeResumeSession) SendInput(ctx context.Context, data []byte) error {
	var err error
	defer func() { s.done <- err }()
	select {
	case <-s.release:
	case <-ctx.Done():
		err = ctx.Err()
		return err
	}
	err = s.Session.SendInput(ctx, data)
	return err
}
