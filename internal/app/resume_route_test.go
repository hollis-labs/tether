package app

import (
	"context"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
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
			request := CreateSessionInput{LaunchID: "codex-launch"}
			if routed {
				request.Route = &launchprofile.Route{Channel: "override-channel", Kinds: []string{"question", "failure"}}
			}
			parent, err := rig.svc.CreateSessionWithInput(request)
			if err != nil {
				t.Fatal(err)
			}
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
			t.Cleanup(func() { _ = rig.svc.Manager.Stop(context.Background(), resumed.SessionID) })
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
