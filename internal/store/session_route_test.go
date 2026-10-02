package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestSessionRouteSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    string
		route *launchprofile.Route
	}{
		{id: "unrouted"}, {id: "routed", route: &launchprofile.Route{Channel: "ops"}}, {id: "empty", route: &launchprofile.Route{Channel: "ops", Kinds: []string{}}},
	} {
		if err := db.CreateSession(SessionRow{ID: tc.id, State: "created"}, &launch.Plan{Route: tc.route}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateSession(SessionRow{ID: "legacy", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		id   string
		want *launchprofile.Route
	}{
		{id: "unrouted"}, {id: "legacy"},
		{id: "routed", want: &launchprofile.Route{Channel: "ops", Kinds: []string{"final", "question", "approval", "failure"}}},
		{id: "empty", want: &launchprofile.Route{Channel: "ops", Kinds: []string{}}},
	} {
		route, err := db.SessionRoute(context.Background(), tc.id)
		if err != nil || !reflect.DeepEqual(route, tc.want) {
			t.Fatalf("%s: route=%+v, err=%v", tc.id, route, err)
		}
		plan, err := db.GetLaunchPlan(tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(plan.Route, tc.want) {
			t.Fatalf("%s: plan route=%+v", tc.id, plan.Route)
		}
	}
	if _, err := db.SessionRoute(context.Background(), "missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestInvalidRouteNotPersisted(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.CreateSession(SessionRow{ID: "invalid", State: "created"}, &launch.Plan{Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"bogus"}}})
	var invalid *launchprofile.InvalidRouteError
	if !errors.As(err, &invalid) {
		t.Fatalf("want typed error: %v", err)
	}
	if _, err := db.GetSession("invalid"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("invalid session persisted: %v", err)
	}
}
