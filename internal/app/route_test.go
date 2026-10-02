package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestApplyAgentOpsRoute(t *testing.T) {
	for _, tc := range []struct {
		name, agentInline, override, channel string
		api                                  *launchprofile.Route
		invalid                              bool
	}{
		{name: "catalog", channel: "catalog"},
		{name: "inline", agentInline: `{"route":{"channel":"inline"}}`, channel: "inline"},
		{name: "override", agentInline: `{"route":{"channel":"inline"}}`, override: `{"route":{"channel":"override"}}`, channel: "override"},
		{name: "api wins", override: `{"route":{"channel":"override"}}`, api: &launchprofile.Route{Channel: "api"}, channel: "api"},
		{name: "invalid kinds", override: `{"route":{"channel":"ops","kinds":["terminal"]}}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := config.Agent{ID: "test-agent", Route: &launchprofile.Route{Channel: "catalog"}}
			svc := buildTestService(t, map[string]config.Agent{"test-agent": agent}, t.TempDir())
			plan := basePlan()
			plan.Route = agent.Route
			err := svc.applyAgentOps(plan, CreateSessionInput{AgentInline: tc.agentInline, Override: tc.override, Route: tc.api})
			if tc.invalid {
				var invalid *launchprofile.InvalidRouteError
				if !errors.As(err, &invalid) {
					t.Fatalf("want typed route error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if plan.Route.Channel != tc.channel || !reflect.DeepEqual(plan.Route.Kinds, []string{"final", "question", "approval", "failure"}) {
				t.Fatalf("route = %+v", plan.Route)
			}
		})
	}
}

func TestCreateSessionWithoutRouteUnchanged(t *testing.T) {
	svc, _ := idemHarness(t)
	plan, err := idemPlan(t)()
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.createSessionFromPlan(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	route, err := svc.Store.SessionRoute(context.Background(), result.SessionID)
	if err != nil || route != nil || result.Plan.Route != nil {
		t.Fatalf("default acquired routing: %+v, %v", route, err)
	}
	row, err := svc.Store.GetSession(result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "created" || row.RouteJSON.Valid {
		t.Fatalf("default session changed: %+v", row)
	}
	stored, err := svc.Store.GetLaunchPlan(result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, stored) {
		t.Fatalf("unrouted plan changed: %+v", stored)
	}
}

func TestRouteChangesIdempotencyDigest(t *testing.T) {
	original := CreateSessionInput{LaunchID: "demo"}
	routed := original
	routed.Route = &launchprofile.Route{Channel: "ops"}
	if createRequestDigest(original) == createRequestDigest(routed) {
		t.Fatal("route missing from digest")
	}
	other := routed
	other.Route = &launchprofile.Route{Channel: "ops", Kinds: []string{"question"}}
	if createRequestDigest(other) == createRequestDigest(routed) {
		t.Fatal("kinds missing from digest")
	}
}
