package main

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
)

// Factory overrides stand in for app.NewDaemon, whose startup contract includes
// a frozen identity before returning any service to event-producing bootstrap.
// Use the real descriptor's selected DB/file identity, never a synthetic UUID.
func seedDaemonFixtureEventIdentity(t *testing.T, svc *app.Service) {
	t.Helper()
	descriptor, err := buildEnvironmentDescriptor(svc.Catalog, svc.Store, nil)
	if err != nil {
		t.Fatal("fixture environment descriptor", err)
	}
	id, _, _ := descriptor.Identity()
	progress, err := svc.Store.SeedEventIdentity(context.Background(), id)
	if err != nil {
		t.Fatal("fixture event identity", err)
	}
	if progress.EnvironmentID != id {
		t.Fatal("fixture descriptor and event identity differ")
	}
}
