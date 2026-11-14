package registry_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestBackfillHandlesFromCatalogIDs(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	writeBootstrapFile(t, filepath.Join(root, "agents"), "photon-tether.yaml", "id: photon-tether\nname: Photon Tether\n")
	writeBootstrapFile(t, filepath.Join(root, "agents"), "Odd_Name.yaml", "id: Odd Name\nname: Odd\n")
	writeBootstrapFile(t, filepath.Join(root, "agents"), "cmd-builder-1.yaml", "id: cmd-builder-1\nname: Builder\n")
	writeBootstrapFile(t, filepath.Join(root, "agents"), "chrispian.yaml", "id: chrispian\nname: Impostor\n")
	writeBootstrapFile(t, filepath.Join(root, "agents"), "sneaky.yaml", "id: sneaky\nname: Sneaky\n")
	if _, err := registry.BootstrapFromCatalog(ctx, svc, root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BackfillTetherExternalIDs(ctx, svc, root); err != nil {
		t.Fatal(err)
	}
	n, err := registry.BackfillHandles(ctx, svc, map[string]string{"cmd-builder-1": "builder-1", "sneaky": "chrispian"})
	if err != nil || n != 2 {
		t.Fatalf("attached %d, err %v; want 2 (the invalid and the reserved names are skipped)", n, err)
	}
	if _, err := svc.LookupBy(ctx, registry.KindAgent, registry.ReservedHandle, registry.HandleSubstrate); err == nil {
		t.Fatal("an agent holds the reserved operator handle")
	}
	if _, err := svc.LookupBy(ctx, registry.KindAgent, "builder-1", registry.HandleSubstrate); err != nil {
		t.Fatalf("catalog handle override not attached: %v", err)
	}
	p, err := svc.LookupBy(ctx, registry.KindAgent, "photon-tether", registry.HandleSubstrate)
	if err != nil || p.URN == "" {
		t.Fatalf("lookup by handle: %v", err)
	}
	// Idempotent.
	if n, err := registry.BackfillHandles(ctx, svc, map[string]string{"cmd-builder-1": "builder-1", "sneaky": "chrispian"}); err != nil || n != 0 {
		t.Fatalf("second pass attached %d, err %v", n, err)
	}
}
