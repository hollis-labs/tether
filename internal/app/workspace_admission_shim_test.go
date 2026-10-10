//go:build linux

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/harness/runner"
	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimhost"
)

func TestLaunchScratchShimPrepareSubstitutionRefusesPlacement(t *testing.T) {
	for _, scenario := range []string{"accepted", "physical-substitution", "changed-row", "changed-raw-plan", "changed-holder"} {
		t.Run(scenario, func(t *testing.T) {
			f := shimFixture(t)
			ctx := context.Background()
			unlock, err := f.svc.lockSessionLaunch(ctx, f.req.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			accepted, err := f.svc.Store.WorkspaceLaunchDecision(ctx, f.req.ID)
			if err != nil {
				t.Fatal(err)
			}
			var databasePath string
			if err := f.svc.Store.DB().QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&databasePath); err != nil {
				t.Fatal(err)
			}
			custody, err := f.svc.admitLaunchScratch(ctx, accepted, f.plan, uuid.NewString(), []string{filepath.Dir(databasePath)})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = custody.Close() }()
			f.req.Runtime = &launchScratchRuntime{Runtime: f.req.Runtime, custody: custody}
			f.req.Options.Env = mergeEnv(f.req.Options.Env, map[string]string{"TMPDIR": custody.allocation.Root})
			f.svc.shimHosting.prepare = func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, _ runner.ResourceLimits) (shim.Launch, func(), error) {
				switch scenario {
				case "physical-substitution":
					if err := os.Rename(custody.allocation.Root, custody.allocation.Root+"-retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(custody.allocation.Root, 0o700); err != nil {
						t.Fatal(err)
					}
				case "changed-row":
					if _, err := f.svc.Store.DB().Exec("UPDATE sessions SET intent='fork' WHERE id=?", f.req.ID); err != nil {
						t.Fatal(err)
					}
				case "changed-raw-plan":
					if _, err := f.svc.Store.DB().Exec("UPDATE launch_plans SET plan_json=json_set(plan_json,'$.future_authority','changed') WHERE session_id=?", f.req.ID); err != nil {
						t.Fatal(err)
					}
				case "changed-holder":
					unlock()
					unlock = nil
				}

				return spec, func() {}, nil
			}
			placements := 0
			f.svc.shimHosting.place = func(_ context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
				placements++
				// A real placement callback may read Store; no transaction may span it.
				if _, err := f.svc.Store.WorkspaceLaunchDecision(ctx, f.req.ID); err != nil {
					t.Fatal(err)
				}
				receipt := f.svc.shimHosting.provider.PlacementIdentity(key, spec)
				receipt.Attempted = true
				return receipt, &shimhost.Failure{Code: "outcome_unknown", Message: "synthetic placement witness"}
			}
			if _, err := f.svc.prepareShimStart(ctx, f.plan, f.req); err == nil {
				t.Fatal("substituted scratch unexpectedly admitted")
			}
			expected := 0
			if scenario == "accepted" {
				expected = 1
			}
			if placements != expected {
				t.Fatal("prepare substitution reached provider placement")
			}
		})
	}
}
