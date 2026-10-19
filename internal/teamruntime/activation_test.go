package teamruntime

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/teamactivation"
	"github.com/hollis-labs/tether/internal/teamspolicy"
)

func TestActivationBaselinePersistsAndChangedLaunchIsNotAttested(t *testing.T) {
	root := t.TempDir()
	db, storage, reg := dbFixture(t, filepath.Join(root, "state.db"))
	raw := []byte("version: 1\nlaunches:\n- launch: worker-launch\n  runtime: cli\n  definition: {id: worker, revision: r1, digest: verified-digest}\n")
	check(t, os.WriteFile(teamspolicy.Path(root), raw, 0600))
	policy, err := teamspolicy.Load(root)
	check(t, err)
	cat := &config.Catalog{Projects: map[string]config.Project{"p": {ID: "p", RepoRoot: t.TempDir()}}, Agents: map[string]config.Agent{"a": {ID: "a"}}, Providers: map[string]config.Provider{"cli": {ID: "cli", Type: "cli", Command: "echo"}}, Launches: map[string]config.Launch{"worker-launch": {ID: "worker-launch", Agent: "a", Project: "p", Provider: "cli"}}}
	svc := &app.Service{Catalog: cat, Store: db} // in-memory synthetic catalog, never reads user's state
	e := enroller(t, db, storage, reg)
	inspector := &ActivationInspector{Policy: policy, Service: svc, Definitions: &verifiedDefinitions{}, Enroller: e}
	q := teamactivation.Query{LaunchID: "worker-launch"}
	before, err := inspector.Inspect(t.Context(), q)
	check(t, err)
	if before.BaselineState != "unknown" {
		t.Fatal("missing historical baseline invented")
	}
	inspector.CaptureLoaded(t.Context())
	before, err = inspector.Inspect(t.Context(), q)
	check(t, err)
	if before.BaselineState != "recorded" {
		t.Fatal("baseline not recorded")
	}
	changed := cat.Launches["worker-launch"]
	changed.Worker = "other-worker"
	cat.Launches["worker-launch"] = changed
	restarted := &ActivationInspector{Policy: policy, Service: svc, Definitions: &verifiedDefinitions{}, Enroller: e}
	restarted.CaptureLoaded(t.Context())
	after, err := restarted.Inspect(t.Context(), q)
	check(t, err)
	if !slices.Contains(after.Divergence, "launch_changed_since_load") || after.Pin != before.Pin || after.LoadedBaseline.Fingerprint != before.LoadedBaseline.Fingerprint {
		t.Fatal("unchanged pin or restart hid changed launch")
	}
	e.InspectLaunch = svc.InspectTeamLaunch
	_, err = e.Ensure(t.Context(), fresh("admitted-evidence"))
	check(t, err)
	receipt, err := e.read(t.Context(), "enrollment", "admitted-evidence")
	check(t, err)
	if len(receipt.payload) == 0 {
		t.Fatal("missing admission receipt")
	}
}
