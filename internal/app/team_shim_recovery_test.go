//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

func goneTeamShimRig(t *testing.T) (*codexRig, store.SessionShimRow, shimhost.Receipt, *shimHosting, string) {
	t.Helper()
	r, id, actor := retainedRecoveryRig(t)
	if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET state='detached',exit_code=NULL,ended_at=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "placement")
	if err := shimhost.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	receipt := shimhost.Receipt{Session: id, OperationKey: "owned-gone", Instance: "fixture", Generation: 1, Backend: "detached", DescriptorPath: filepath.Join(dir, "launch.json"), SocketPath: filepath.Join(dir, "control.sock"), Journal: "retained-journal", HostPID: 100, ShimPID: 100, ProviderPID: 200, Attempted: true}
	if err := shimhost.WritePrivateJSON(filepath.Join(dir, "placement.json"), receipt); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.persistShim(context.Background(), id, "claude", "original-boot", receipt); err != nil {
		t.Fatal(err)
	}
	row, err := r.svc.Store.SessionShim(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	host := &shimHosting{stop: func(_ context.Context, current shimhost.Receipt) error {
		current.Retired = true
		return shimhost.WritePrivateJSON(filepath.Join(dir, "placement.json"), current)
	}}
	return r, row, receipt, host, actor
}

// Custody/process absence is an inert synthetic host proof around the real
// direct Codex fixture. This exercises the shared state/admission boundary;
// the production Codex Gone branch still refuses unresolved private inboxes.
func TestGoneTeamShimRetirementPreservesSameIDAuthority(t *testing.T) {
	r, row, receipt, host, actor := goneTeamShimRig(t)
	ctx := context.Background()
	before, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.svc.recoverGoneTeamShim(ctx, row, receipt, host, func(pid int) bool { return pid == 100 || pid == 200 }); err != nil {
		t.Fatal(err)
	}
	if _, err = r.svc.Store.SessionShim(ctx, row.SessionID); !errors.Is(err, store.ErrSessionShimNotFound) {
		t.Fatal("old active custody still dispatchable", err)
	}
	after, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil || before.ID != after.ID || before.Generation != after.Generation || after.SessionID != row.SessionID {
		t.Fatalf("authority changed: %+v %v", after, err)
	}
	session, err := r.svc.Store.GetSession(row.SessionID)
	if err != nil || session.State != "orphaned" || session.PID.Valid || session.ExitCode.Valid || session.EndedAt.Valid {
		t.Fatalf("pending state: %+v %v", session, err)
	}
	var raw, state, binding string
	if err = r.svc.Store.DB().QueryRow(`SELECT custody_json,state,binding_id FROM team_gone_shim_recoveries WHERE session_id=?`, row.SessionID).Scan(&raw, &state, &binding); err != nil {
		t.Fatal(err)
	}
	var archived store.SessionShimRow
	if json.Unmarshal([]byte(raw), &archived) != nil || archived != row || state != "recovery_pending" || binding != before.ID {
		t.Fatal("exact custody/fence not retained")
	}
	if n := r.svc.revokeEndedSessionBindings(ctx); n != 0 {
		t.Fatal("startup revoked eligible pending binding", n)
	}
	// The existing same-ID native path now sees no active old shim row.
	if err = r.svc.RecoverTeamSession(ctx, "retained", row.SessionID); err != nil {
		t.Fatal(err)
	}
	r.wait(2)
	r.idle(row.SessionID)
	final, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil || final.ID != before.ID || final.Generation != before.Generation || final.SessionID != row.SessionID {
		t.Fatal("native admission minted authority", err)
	}
}

func TestGoneTeamShimRefusesUncertainAndChangedCustody(t *testing.T) {
	for _, name := range []string{"provider live", "host live", "unknown provider", "changed receipt", "not retired", "revoked binding", "explicit stop", "terminal session", "changed custody", "binding changes during retirement", "custody changes during retirement", "transaction failure"} {
		t.Run(name, func(t *testing.T) {
			r, row, receipt, host, actor := goneTeamShimRig(t)
			ctx := context.Background()
			absent := func(pid int) bool { return pid == 100 || pid == 200 }
			stopCalls := 0
			originalStop := host.stop
			host.stop = func(ctx context.Context, current shimhost.Receipt) error {
				stopCalls++
				return originalStop(ctx, current)
			}
			db := r.svc.Store.DB()
			switch name {
			case "provider live":
				absent = func(pid int) bool { return pid == 100 }
			case "host live":
				absent = func(pid int) bool { return pid == 200 }
			case "unknown provider":
				receipt.ProviderPID = 0
				if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(row.DescriptorPath), "placement.json"), receipt); err != nil {
					t.Fatal(err)
				}
			case "changed receipt":
				receipt.Fingerprint = "changed"
			case "not retired":
				host.stop = func(context.Context, shimhost.Receipt) error { stopCalls++; return nil }
			case "revoked binding":
				if _, err := db.Exec(`UPDATE runtime_bindings SET revoked_at='revoked' WHERE target_urn=?`, actor); err != nil {
					t.Fatal(err)
				}
			case "explicit stop":
				if _, err := db.Exec(`UPDATE team_port_intents SET ended='stop' WHERE port_kind='session'`); err != nil {
					t.Fatal(err)
				}
			case "terminal session":
				if _, err := db.Exec(`UPDATE sessions SET state='killed' WHERE id=?`, row.SessionID); err != nil {
					t.Fatal(err)
				}
			case "transaction failure":
				if _, err := db.Exec(`CREATE TRIGGER reject_gone_pending BEFORE UPDATE OF state ON sessions WHEN NEW.state='orphaned' BEGIN SELECT RAISE(ABORT,'inert storage failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "changed custody":
				if _, err := db.Exec(`UPDATE session_shims SET boot_generation='changed'`); err != nil {
					t.Fatal(err)
				}
			case "binding changes during retirement":
				host.stop = func(ctx context.Context, current shimhost.Receipt) error {
					stopCalls++
					if _, err := db.Exec(`UPDATE runtime_bindings SET revoked_at='revoked' WHERE target_urn=?`, actor); err != nil {
						return err
					}
					return originalStop(ctx, current)
				}
			case "custody changes during retirement":
				host.stop = func(ctx context.Context, current shimhost.Receipt) error {
					stopCalls++
					if _, err := db.Exec(`UPDATE session_shims SET controller_epoch='1'`); err != nil {
						return err
					}
					return originalStop(ctx, current)
				}
			}
			if err := r.svc.recoverGoneTeamShim(ctx, row, receipt, host, absent); err == nil {
				t.Fatal("unsafe custody retired")
			}
			if _, err := r.svc.Store.SessionShim(ctx, row.SessionID); err != nil {
				t.Fatal("refused custody removed", err)
			}
			var archives int
			if err := db.QueryRow(`SELECT COUNT(*) FROM team_gone_shim_recoveries WHERE session_id=?`, row.SessionID).Scan(&archives); err != nil || archives != 0 {
				t.Fatal("refusal committed pending recovery", err)
			}
			if name != "not retired" && name != "binding changes during retirement" && name != "custody changes during retirement" && name != "transaction failure" && stopCalls != 0 {
				t.Fatal("refusal reached retirement")
			}
		})
	}
}

func TestRecordedPIDAbsenceIsPositiveOnly(t *testing.T) {
	if recordedPIDAbsent(0) || recordedPIDAbsent(-1) || recordedPIDAbsent(1) {
		t.Fatal("unknown/live PID treated as absent")
	}
}

func TestGoneTeamShimReconcileUsesPendingTransition(t *testing.T) {
	r, row, receipt, host, actor := goneTeamShimRig(t)
	t.Setenv(EnvLaunchHost, "shim")
	// Positive process absence is earned from two reaped owned fixture children.
	for _, pid := range []*int{&receipt.HostPID, &receipt.ProviderPID} {
		child := exec.Command("/bin/true")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		*pid = child.Process.Pid
		if err := child.Wait(); err != nil {
			t.Fatal(err)
		}
		if !recordedPIDAbsent(*pid) {
			t.Skip("owned reaped PID was reused")
		}
	}
	receipt.ShimPID = receipt.HostPID
	if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(row.DescriptorPath), "placement.json"), receipt); err != nil {
		t.Fatal(err)
	}
	row.HostPID = receipt.HostPID
	row.ShimPID = receipt.ShimPID
	row.ProviderPID = receipt.ProviderPID
	if err := r.svc.Store.UpsertSessionShim(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	// This is the non-Codex production branch. The Codex fixture only supplies
	// the retained identity/SQL setup; no Codex inbox proof is being exercised.
	if _, err := r.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.provider_brand','claude') WHERE session_id=?`, row.SessionID); err != nil {
		t.Fatal(err)
	}
	host.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
		return shimhost.Inspection{Gone: true}, nil
	}
	r.svc.shimHosting = host
	before, err := r.svc.Registry.CurrentBinding(context.Background(), actor)
	if err != nil {
		t.Fatal(err)
	}
	r.svc.ReconcileStaleState()
	current, err := r.svc.Store.GetSession(row.SessionID)
	if err != nil || current.State != "orphaned" {
		t.Fatalf("Gone transition: %+v %v", current, err)
	}
	if _, err := r.svc.Store.SessionShim(context.Background(), row.SessionID); !errors.Is(err, store.ErrSessionShimNotFound) {
		t.Fatal("old custody retained", err)
	}
	after, err := r.svc.Registry.CurrentBinding(context.Background(), actor)
	if err != nil || before.ID != after.ID || before.Generation != after.Generation {
		t.Fatal("reconcile used revoke-all path", err)
	}
}
