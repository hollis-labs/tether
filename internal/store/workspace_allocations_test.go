package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/launch"
)

func scratchStoreFixture(t *testing.T) (*Store, SessionRow, *launch.Plan) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan := &launch.Plan{LaunchID: "launch", ProjectID: "project", ProviderID: "codex", WorkRoot: t.TempDir(), Env: map[string]string{"KEEP": "accepted"}}
	row := SessionRow{ID: "scratch-session", LaunchID: "launch", ProjectID: "project", ProviderID: "codex", State: "created", Workspace: t.TempDir()}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	storedRow, err := db.GetSession(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedPlan, err := db.GetLaunchPlan(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	return db, *storedRow, storedPlan
}

func reserveScratchStoreFixture(ctx context.Context, t *testing.T, db *Store, row SessionRow, plan *launch.Plan, operation string) (WorkspaceScratchAllocation, bool, error) {
	t.Helper()
	decision, err := db.WorkspaceLaunchDecision(ctx, row.ID)
	if err != nil {
		return WorkspaceScratchAllocation{}, false, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	decision.Session, decision.PlanJSON = row, string(raw)
	return db.ReservePreparedWorkspaceScratch(ctx, decision, operation)
}

func TestWorkspacePreparationExactSnapshotAndAtomicRefusal(t *testing.T) {
	for _, scenario := range []string{"accepted", "changed-row", "changed-raw-plan", "native-stamp-failure", "native-stamp-ignored", "ref-stamp-best-effort", "ref-stamp-ignored"} {
		t.Run(scenario, func(t *testing.T) {
			db, row, _ := scratchStoreFixture(t)
			ctx := context.Background()
			before, err := db.WorkspaceLaunchDecision(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			stamp := &WorkspacePreparationStamp{RefAttribution: RefAttributionNone, NativeStateRoot: t.TempDir()}
			switch scenario {
			case "changed-row":
				_, err = db.DB().Exec("UPDATE sessions SET intent='fork' WHERE id=?", row.ID)
			case "changed-raw-plan":
				_, err = db.DB().Exec("UPDATE launch_plans SET plan_json=json_set(plan_json,'$.future_authority','different') WHERE session_id=?", row.ID)
			case "native-stamp-failure":
				_, err = db.DB().Exec(`CREATE TRIGGER reject_native_stamp BEFORE UPDATE ON launch_plans BEGIN SELECT RAISE(ABORT,'synthetic native failure'); END`)
			case "ref-stamp-best-effort":
				_, err = db.DB().Exec(`CREATE TRIGGER reject_ref_stamp BEFORE UPDATE OF ref_attribution ON sessions BEGIN SELECT RAISE(ABORT,'synthetic audit failure'); END`)
			case "native-stamp-ignored":
				_, err = db.DB().Exec(`CREATE TRIGGER ignore_native_stamp BEFORE UPDATE ON launch_plans BEGIN SELECT RAISE(IGNORE); END`)
			case "ref-stamp-ignored":
				_, err = db.DB().Exec(`CREATE TRIGGER ignore_ref_stamp BEFORE UPDATE OF ref_attribution ON sessions BEGIN SELECT RAISE(IGNORE); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			preTransition, err := db.WorkspaceLaunchDecision(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			result, transitionErr := db.RecordWorkspacePreparation(ctx, before, stamp)
			actual, err := db.WorkspaceLaunchDecision(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "changed-row" || scenario == "changed-raw-plan" || scenario == "native-stamp-failure" || scenario == "native-stamp-ignored" {
				if transitionErr == nil || !reflect.DeepEqual(actual, preTransition) {
					t.Fatalf("refused preparation applied a stamp: err=%v", transitionErr)
				}
				return
			}
			if transitionErr != nil || !reflect.DeepEqual(result.Decision, actual) {
				t.Fatalf("computed snapshot differs: err=%v", transitionErr)
			}
			if scenario == "ref-stamp-best-effort" || scenario == "ref-stamp-ignored" {
				if result.RefAttributionError == nil || actual.Session.RefAttribution != before.Session.RefAttribution || actual.Session.UpdatedAt != before.Session.UpdatedAt {
					t.Fatal("best-effort attribution error behavior changed")
				}
			} else if result.RefAttributionError != nil || actual.Session.RefAttribution.String != stamp.RefAttribution || actual.Session.UpdatedAt == "" {
				t.Fatal("computed attribution stamp missing")
			}
			plan, err := db.GetLaunchPlan(row.ID)
			if err != nil || plan.NativeStateRoot != stamp.NativeStateRoot {
				t.Fatalf("exact computed native root lost: %v", err)
			}
			if _, _, err := db.ReservePreparedWorkspaceScratch(ctx, result.Decision, uuid.NewString()); err != nil {
				t.Fatalf("exact post-preparation JSON refused: %v", err)
			}
		})
	}
}

func TestWorkspaceScratchReceiptReplayAndRollback(t *testing.T) {
	db, row, plan := scratchStoreFixture(t)
	ctx := context.Background()
	op := uuid.NewString()
	reserved, replay, err := reserveScratchStoreFixture(ctx, t, db, row, plan, op)
	if err != nil || replay {
		t.Fatalf("reserve: replay=%v err=%v", replay, err)
	}
	identity := ScratchPhysicalIdentity{Workspace: "device:parent", Scratch: "device:child"}
	refusal := errors.New("physical custody changed")
	if _, err := db.AdmitWorkspaceScratch(ctx, reserved, identity, func(context.Context) error { return refusal }); !errors.Is(err, refusal) {
		t.Fatalf("failed custody: %v", err)
	}
	stillReserved, replay, err := reserveScratchStoreFixture(ctx, t, db, row, plan, op)
	if err != nil || !replay || stillReserved != reserved {
		t.Fatalf("rollback lost retained reservation: %+v replay=%v err=%v", stillReserved, replay, err)
	}
	admitted, err := db.AdmitWorkspaceScratch(ctx, reserved, identity, func(context.Context) error { return nil })
	if err != nil || admitted.Status != "admitted" || admitted.PhysicalIdentity != identity {
		t.Fatalf("admit: %+v err=%v", admitted, err)
	}
	actual, replay, err := reserveScratchStoreFixture(ctx, t, db, row, plan, op)
	if err != nil || !replay || actual != admitted {
		t.Fatalf("admitted replay: %+v replay=%v err=%v", actual, replay, err)
	}
	if _, _, err := reserveScratchStoreFixture(ctx, t, db, row, plan, uuid.NewString()); !errors.Is(err, ErrWorkspaceAllocationConflict) {
		t.Fatalf("different nonce: %v", err)
	}
	if _, err := db.AdmitWorkspaceScratch(ctx, reserved, identity, func(context.Context) error { t.Fatal("admitted twice"); return nil }); !errors.Is(err, ErrWorkspaceAllocationConflict) {
		t.Fatalf("duplicate transition: %v", err)
	}
	storedPlan, err := db.GetLaunchPlan(row.ID)
	if err != nil || storedPlan.Env["KEEP"] != "accepted" || storedPlan.WorkRoot != plan.WorkRoot {
		t.Fatalf("accepted plan altered: %+v err=%v", storedPlan, err)
	}
}

func TestWorkspaceScratchPlacementUsesExactTransition(t *testing.T) {
	for _, scenario := range []string{"accepted", "row-before-transition", "unrelated-trigger-change", "audit-time-after-transition", "raw-plan-after-transition"} {
		t.Run(scenario, func(t *testing.T) {
			db, row, plan := scratchStoreFixture(t)
			ctx := context.Background()
			decision, err := db.WorkspaceLaunchDecision(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			reserved, _, err := reserveScratchStoreFixture(ctx, t, db, row, plan, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			allocation, err := db.AdmitWorkspaceScratch(ctx, reserved, ScratchPhysicalIdentity{Workspace: "parent", Scratch: "child"}, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "row-before-transition":
				_, err = db.DB().Exec("UPDATE sessions SET intent='fork' WHERE id=?", row.ID)
			case "unrelated-trigger-change":
				_, err = db.DB().Exec(`CREATE TRIGGER change_launch_intent AFTER UPDATE OF state ON sessions BEGIN UPDATE sessions SET intent='fork' WHERE id=NEW.id; END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := db.WorkspaceLaunchDecision(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			transition, transitionErr := db.BeginWorkspaceScratchPlacement(ctx, decision, allocation)
			if scenario == "row-before-transition" || scenario == "unrelated-trigger-change" {
				actual, err := db.WorkspaceLaunchDecision(ctx, row.ID)
				if err != nil || !errors.Is(transitionErr, ErrWorkspaceAllocationConflict) || !reflect.DeepEqual(before, actual) {
					t.Fatalf("refused transition mutated state: %v %v", transitionErr, err)
				}
				return
			}
			if transitionErr != nil || transition.Session.State != "launching" || transition.PlanJSON != decision.PlanJSON {
				t.Fatalf("transition: %+v %v", transition, transitionErr)
			}
			switch scenario {
			case "audit-time-after-transition":
				_, err = db.DB().Exec("UPDATE sessions SET updated_at='2000-01-01T00:00:00Z' WHERE id=?", row.ID)
			case "raw-plan-after-transition":
				_, err = db.DB().Exec("UPDATE launch_plans SET plan_json=json_set(plan_json,'$.unknown_field','changed') WHERE session_id=?", row.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			validationErr := db.ValidateWorkspaceScratchPlacement(ctx, transition, allocation)
			if (validationErr != nil) != (scenario != "accepted") {
				t.Fatalf("exact placement predicate: %v", validationErr)
			}
		})
	}
}

func TestWorkspacePreparationAdmittedRetryNeverRefreshesSnapshot(t *testing.T) {
	db, row, _ := scratchStoreFixture(t)
	ctx := context.Background()
	before, err := db.WorkspaceLaunchDecision(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	stamp := &WorkspacePreparationStamp{RefAttribution: RefAttributionNone, NativeStateRoot: t.TempDir()}
	prepared, err := db.RecordWorkspacePreparation(ctx, before, stamp)
	if err != nil {
		t.Fatal(err)
	}
	reserved, _, err := db.ReservePreparedWorkspaceScratch(ctx, prepared.Decision, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdmitWorkspaceScratch(ctx, reserved, ScratchPhysicalIdentity{Workspace: "parent", Scratch: "child"}, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	retry, err := db.RecordWorkspacePreparation(ctx, prepared.Decision, stamp)
	if err != nil || !reflect.DeepEqual(retry.Decision, prepared.Decision) {
		t.Fatalf("retry refreshed snapshot: %v", err)
	}
	changed := *stamp
	changed.NativeStateRoot = t.TempDir()
	if _, err := db.RecordWorkspacePreparation(ctx, prepared.Decision, &changed); !errors.Is(err, ErrWorkspaceAllocationConflict) {
		t.Fatalf("changed computed stamp admitted: %v", err)
	}
	actual, err := db.WorkspaceLaunchDecision(ctx, row.ID)
	if err != nil || !reflect.DeepEqual(actual, prepared.Decision) {
		t.Fatal("refused retry changed stored preparation")
	}
}

func TestWorkspaceScratchReceiptRejectsChangedDecision(t *testing.T) {
	for _, change := range []string{"state", "pid", "pid-start", "workspace", "plan", "unknown-plan-field", "operation", "root", "session-digest", "plan-digest"} {
		t.Run(change, func(t *testing.T) {
			db, row, plan := scratchStoreFixture(t)
			ctx := context.Background()
			reserved, _, err := reserveScratchStoreFixture(ctx, t, db, row, plan, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "state":
				err = db.UpdateSessionState(row.ID, "running", 0, nil)
			case "pid":
				_, err = db.DB().Exec("UPDATE sessions SET pid=7 WHERE id=?", row.ID)
			case "pid-start":
				_, err = db.DB().Exec("UPDATE sessions SET pid_started_at='2000-01-01T00:00:00Z' WHERE id=?", row.ID)
			case "workspace":
				_, err = db.DB().Exec("UPDATE sessions SET workspace=? WHERE id=?", t.TempDir(), row.ID)
			case "plan":
				_, err = db.DB().Exec("UPDATE launch_plans SET plan_json=json_set(plan_json,'$.work_root',?) WHERE session_id=?", t.TempDir(), row.ID)
			case "unknown-plan-field":
				_, err = db.DB().Exec("UPDATE launch_plans SET plan_json=json_set(plan_json,'$.unrecognized_policy','changed') WHERE session_id=?", row.ID)
			case "operation":
				reserved.OperationID = uuid.NewString()
			case "root":
				reserved.Root = t.TempDir()
			case "session-digest":
				reserved.SessionDigest = "changed"
			case "plan-digest":
				reserved.PlanDigest = "changed"
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.AdmitWorkspaceScratch(ctx, reserved, ScratchPhysicalIdentity{Workspace: "parent", Scratch: "child"}, func(context.Context) error {
				t.Fatal("changed decision reached physical admission")
				return nil
			}); !errors.Is(err, ErrWorkspaceAllocationConflict) {
				t.Fatalf("changed decision: %v", err)
			}
			var status, identity string
			if err := db.DB().QueryRow("SELECT status,physical_identity FROM workspace_scratch_allocations WHERE session_id=?", row.ID).Scan(&status, &identity); err != nil {
				t.Fatal(err)
			}
			if status != "reserved" || identity != "" {
				t.Fatal("failed transition changed allocation")
			}
		})
	}
}

func TestWorkspaceScratchConcurrentReservation(t *testing.T) {
	db, row, plan := scratchStoreFixture(t)
	var path string
	if err := db.DB().QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, store := range []*Store{db, other} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, _, err := reserveScratchStoreFixture(context.Background(), t, store, row, plan, uuid.NewString())
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrWorkspaceAllocationConflict):
			conflict++
		default:
			t.Fatalf("reservation error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("reservation winners=%d conflicts=%d", success, conflict)
	}
}
