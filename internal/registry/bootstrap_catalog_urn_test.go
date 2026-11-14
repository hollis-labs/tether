package registry_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

// A catalog agent with urn: X is X: bootstrap attaches its tether id to X (and
// the handle backfill its handle) and mints no duplicate; an urn: that is not
// registered is refused without minting (CW-20261114-0013).
func TestBootstrapUsesTheCatalogURN(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	seat, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Chief (agent-boot)"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	agents := filepath.Join(root, "agents")
	writeBootstrapFile(t, agents, "chief.yaml", "id: chief\nname: Chief\nurn: "+seat.URN+"\n")
	writeBootstrapFile(t, agents, "ghost.yaml", "id: ghost\nname: Ghost\nurn: msg://agent/agent-mux/agt_notregistered\n")
	writeBootstrapFile(t, agents, "plain.yaml", "id: plain\nname: Plain\n")

	count := func() int {
		rows, err := svc.Search(ctx, registry.KindAgent, registry.Filter{Status: registry.StatusAny})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	before := count()
	report, err := registry.BootstrapFromCatalog(ctx, svc, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BackfillTetherExternalIDs(ctx, svc, root); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BackfillHandles(ctx, svc, nil); err != nil {
		t.Fatal(err)
	}
	// Only "plain" (no urn:) is minted; chief reuses its row; ghost is refused.
	if got := count() - before; got != 1 {
		t.Fatalf("minted %d rows, want 1 (report %+v)", got, report)
	}
	ghostRefused := false
	for _, e := range report.Errors {
		ghostRefused = ghostRefused || (strings.Contains(e.Path, "ghost.yaml") && strings.Contains(e.Reason, "not minting a duplicate"))
	}
	if !ghostRefused {
		t.Fatalf("unregistered catalog urn not refused: %+v", report.Errors)
	}
	for substrate, id := range map[string]string{"tether": "chief", registry.HandleSubstrate: "chief"} {
		p, err := svc.LookupBy(ctx, registry.KindAgent, id, substrate)
		if err != nil || p.URN != seat.URN {
			t.Fatalf("%s=%s -> %q (%v), want the catalog urn %s", substrate, id, p.URN, err, seat.URN)
		}
	}
	// Idempotent: a second run mints nothing.
	n := count()
	if _, err := registry.BootstrapFromCatalog(ctx, svc, root, false); err != nil {
		t.Fatal(err)
	}
	if count() != n {
		t.Fatal("second bootstrap minted rows")
	}
}
