package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
)

func TestIdentityFlipAndRollbackAreAudited(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "catalog")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	global := "# keep me\ndaemon:\n  listen_addr: unix:/tmp/x.sock\nidentity:\n  mode: observe\n"
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	mode := func(point string) string {
		cat, err := config.Load(root)
		if err != nil {
			t.Fatal(err)
		}
		return cat.Global.Identity.ModeFor(point)
	}

	if _, err := applyModeChange(root, "flip", "bogus", "enforce", "t", "e"); err == nil {
		t.Fatal("unknown point accepted")
	}
	if _, err := applyModeChange(root, "flip", "teams", "yes", "t", "e"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	rec, err := applyModeChange(root, "flip", "mcp_gateway", "ENFORCE", "chris", "CW-1 GO")
	if err != nil || rec.From != "observe" || rec.To != "enforce" {
		t.Fatalf("flip: %v %+v", err, rec)
	}
	if mode("mcp_gateway") != "enforce" || mode("teams") != "observe" || mode("local_api") != "observe" {
		t.Fatal("flip was not scoped to one point")
	}
	cat, _ := config.Load(root)
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil || cfg.IdentityModeFor("mcp_gateway") != identity.Enforce || cfg.IdentityModeFor("local_api") != identity.Observe {
		t.Fatalf("daemon config did not resolve per-point mode: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "global.yaml")); !strings.Contains(string(b), "# keep me") {
		t.Fatal("comment lost")
	}

	if _, err := applyModeChange(root, "flip", "global", "enforce", "chris", "CW-1 GO"); err != nil {
		t.Fatal(err)
	}
	if mode("local_api") != "enforce" {
		t.Fatal("global flip not effective")
	}
	for _, p := range append([]string{"global"}, config.IdentityPoints...) {
		if _, err := applyModeChange(root, "rollback", p, "observe", "chris", "regression"); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range config.IdentityPoints {
		if mode(p) != "observe" {
			t.Fatalf("%s still %s after rollback", p, mode(p))
		}
	}
	recs, err := readFlips(filepath.Join(state, "run", "identity-flips.jsonl"))
	if err != nil || len(recs) < 4 || recs[0].Evidence != "CW-1 GO" || recs[0].By != "chris" || recs[0].At == "" {
		t.Fatalf("audit log: %v %+v", err, recs)
	}
	if rec, err := applyModeChange(root, "rollback", "teams", "observe", "chris", "x"); err != nil || rec != nil {
		t.Fatal("rollback of observe point should be a no-op")
	}
}
