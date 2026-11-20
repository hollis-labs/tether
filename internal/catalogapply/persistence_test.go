package catalogapply

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func persistentEngine(t *testing.T) *Engine {
	t.Helper()
	e := newEngine(t)
	e.PlansFile = filepath.Join(filepath.Dir(e.Root), "state", "catalog-plans.json")
	return e
}

func restartEngine(t *testing.T, e *Engine) *Engine {
	t.Helper()
	next := &Engine{Root: e.Root, BackupRoot: e.BackupRoot, Approver: e.Approver,
		PlansFile: e.PlansFile, Validate: e.Validate, Now: e.Now, TTL: e.TTL, RunningSessions: e.RunningSessions}
	if err := next.RestorePlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	return next
}

func persistentPlan(t *testing.T, e *Engine, name string) *Plan {
	t.Helper()
	p, err := e.PlanBundle(context.Background(), Bundle{Name: name, Files: []File{{Path: "agents/" + name + ".yaml", Content: "id: " + name + "\nname: Helper\n"}}}, Options{Caller: Caller{Principal: "session:worker"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlansPersistenceRoundTripAndLiveApprovalOnly(t *testing.T) {
	e := persistentEngine(t)
	p := persistentPlan(t, e, "helper")
	if !p.SurvivesRestart {
		t.Fatal("planned plan must survive restart")
	}
	info, err := os.Stat(e.PlansFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	data, _ := os.ReadFile(e.PlansFile)
	for _, forbidden := range []string{"approved_at", "allow_running", "diff", "privileged", "passphrase"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("stored derived/secret field %s", forbidden)
		}
	}
	next := restartEngine(t, e)
	got, ok := next.Get(p.ID)
	if !ok || got.Digest != p.Digest || got.State != StatePlanned || got.ApprovedAt != nil || !got.ExpiresAt.Equal(p.ExpiresAt) || got.PlannedBy != p.PlannedBy {
		t.Fatalf("restored: %+v", got)
	}
	if _, err := next.Apply(context.Background(), p.ID); err == nil {
		t.Fatal("restart forged approval")
	}
	approve(t, next, got)
	if approved, _ := next.Get(p.ID); approved.SurvivesRestart {
		t.Fatal("approval must be live-only")
	}
	// Approval removes the persisted planned record, never stores an approval.
	again := restartEngine(t, next)
	if _, ok := again.Get(p.ID); ok {
		t.Fatal("approved plan survived restart")
	}
}

func TestPersistedTamperAndStaleBase(t *testing.T) {
	for _, name := range []string{"approved flag", "bundle", "digest", "base", "expiry", "live base", "derived privilege"} {
		t.Run(name, func(t *testing.T) {
			e := persistentEngine(t)
			p := persistentPlan(t, e, "helper")
			raw, _ := os.ReadFile(e.PlansFile)
			var disk map[string]any
			if err := json.Unmarshal(raw, &disk); err != nil {
				t.Fatal(err)
			}
			record := disk["plans"].([]any)[0].(map[string]any)
			switch name {
			case "approved flag":
				record["state"] = "approved"
				record["approved_at"] = time.Now().Format(time.RFC3339)
				record["allow_running"] = true
			case "bundle":
				record["bundle"].(map[string]any)["files"].([]any)[0].(map[string]any)["content"] = "id: helper\npermissions: all\n"
			case "digest":
				record["digest"] = strings.Repeat("0", 64)
			case "base":
				record["base"].(map[string]any)["agents/helper.yaml"] = "fake"
			case "expiry":
				record["expires_at"] = time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339)
			case "live base":
				write(t, e.Root, "agents/helper.yaml", "id: helper\nname: Changed\n")
			case "derived privilege":
				record["operator_only"] = false
				record["privileged"] = []any{}
				record["validation_ok"] = true
			}
			raw, _ = json.Marshal(disk)
			if err := os.WriteFile(e.PlansFile, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			next := restartEngine(t, e)
			got, ok := next.Get(p.ID)
			if name == "approved flag" || name == "derived privilege" {
				if !ok || got.State != StatePlanned || got.ApprovedAt != nil || got.AllowRunning {
					t.Fatalf("disk authority trusted: %+v", got)
				}
				if _, err := next.Apply(context.Background(), p.ID); err == nil {
					t.Fatal("disk forged approval")
				}
			} else if ok {
				t.Fatalf("tampered or stale record restored: %+v", got)
			}
		})
	}
}

func TestPersistenceRebuildsSecurityFlags(t *testing.T) {
	e := persistentEngine(t)
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "trust", Files: []File{{Path: "global.yaml", Content: "version: 1\noperator_attestation:\n  key_id: new\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	next := restartEngine(t, e)
	got, ok := next.Get(p.ID)
	if !ok || !got.OperatorOnly || len(got.Privileged) == 0 {
		t.Fatalf("lost trust-root protection: %+v", got)
	}
	if _, err := next.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Delegated: true, AckPrivileged: true}); err == nil {
		t.Fatal("delegated trust-root approval")
	}
}

func TestPersistenceExpiryGraceAndMaxPlans(t *testing.T) {
	e := persistentEngine(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	p := persistentPlan(t, e, "helper")
	now = p.ExpiresAt.Add(time.Minute)
	next := restartEngine(t, e)
	if _, err := next.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase}); err == nil {
		t.Fatal("expired plan approved")
	}
	rearmed := persistentPlan(t, next, "helper")
	if rearmed.ID != p.ID || !rearmed.ExpiresAt.After(p.ExpiresAt) {
		t.Fatal("rearm lost original id")
	}
	now = rearmed.ExpiresAt.Add(rearmGrace + time.Second)
	next = restartEngine(t, next)
	if _, ok := next.Get(p.ID); ok {
		t.Fatal("plan beyond grace retained")
	}
	for i := 0; i < maxPlans; i++ {
		persistentPlan(t, next, fmt.Sprintf("p%d", i))
	}
	next = restartEngine(t, next)
	if len(next.plans) != maxPlans {
		t.Fatalf("count %d", len(next.plans))
	}
	if _, err := next.PlanBundle(context.Background(), Bundle{Name: "overflow", Files: []File{{Path: "agents/overflow.yaml", Content: "id: overflow\n"}}}, Options{}); err == nil {
		t.Fatal("maxPlans bypass")
	}
	// Existing plans can still be rearmed at capacity.
	persistentPlan(t, next, "p0")
}

func TestPersistenceFailureDoesNotReturnDurablePlan(t *testing.T) {
	e := persistentEngine(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.PlansFile = filepath.Join(blocker, "plans.json")
	_, err := e.PlanBundle(context.Background(), Bundle{Name: "helper", Files: []File{{Path: "agents/helper.yaml", Content: "id: helper\n"}}}, Options{})
	if err == nil || len(e.plans) != 0 {
		t.Fatalf("failed persistence returned a plan: %v", err)
	}
}

func TestPersistenceDoesNotStoreOldSecretsInDiff(t *testing.T) {
	e := persistentEngine(t)
	old := "id: helper\napi_key: sk-abcdefghijklmnopqrstuvwxyz12345678901234567890\n"
	write(t, e.Root, "agents/helper.yaml", old)
	p := persistentPlan(t, e, "helper")
	if !strings.Contains(p.Diff, "api_key") {
		t.Fatal("fixture must put old secret in diff")
	}
	data, _ := os.ReadFile(e.PlansFile)
	if strings.Contains(string(data), "api_key") {
		t.Fatal("persisted secret from old catalog")
	}
}

func TestPersistenceRecomputesDigestBeyondRecordChecksum(t *testing.T) {
	e := persistentEngine(t)
	p := persistentPlan(t, e, "helper")
	raw, _ := os.ReadFile(e.PlansFile)
	var snapshot planFile
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Plans[0].Bundle.Files[0].Content = "id: helper\nname: Forged\n"
	snapshot.Plans[0].Checksum = snapshot.Plans[0].checksum()
	raw, _ = json.Marshal(snapshot)
	if err := os.WriteFile(e.PlansFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	next := restartEngine(t, e)
	if _, ok := next.Get(p.ID); ok {
		t.Fatal("checksum alone accepted edited bundle under original digest")
	}
}

func TestPersistenceRollbackAndSubsecondTTL(t *testing.T) {
	e := persistentEngine(t)
	e.TTL = time.Minute + 500*time.Millisecond
	// A rollback should have its final digest before it is persisted/rearmed.
	backupID := "catalog-pre-original-20261120T120000Z"
	write(t, filepath.Join(e.BackupRoot, backupID), "agents/general.yaml", "id: general\nname: Original\n")
	p, err := e.PlanRollback(context.Background(), backupID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	next := restartEngine(t, e)
	got, ok := next.Get(p.ID)
	if !ok || got.Digest != p.Digest || got.RollbackOf != backupID || !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("rollback restore: %+v", got)
	}
	rearmed, err := next.PlanRollback(context.Background(), backupID, Options{})
	if err != nil || rearmed.ID != p.ID {
		t.Fatalf("rollback rearm: %+v %v", rearmed, err)
	}
}

func TestPersistenceSecretBearingRollbackUsesBackupReferences(t *testing.T) {
	e := persistentEngine(t)
	backupID := "catalog-pre-original-20261120T120000Z"
	write(t, filepath.Join(e.BackupRoot, backupID), "agents/general.yaml", "id: general\napi_key: sk-abcdefghijklmnopqrstuvwxyz12345678901234567890\n")
	p, err := e.PlanRollback(context.Background(), backupID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.SurvivesRestart {
		t.Fatal("secret-bearing rollback must survive by backup reference")
	}
	raw, _ := os.ReadFile(e.PlansFile)
	if strings.Contains(string(raw), "api_key") {
		t.Fatal("secret-bearing rollback content persisted")
	}
	next := restartEngine(t, e)
	got, ok := next.Get(p.ID)
	if !ok || got.State != StatePlanned || got.ApprovedAt != nil || got.Digest != p.Digest {
		t.Fatalf("rollback not restored as unapproved: %+v", got)
	}
	if _, err := next.Apply(context.Background(), p.ID); err == nil {
		t.Fatal("restored rollback bypassed approval")
	}
	// A changed backup cannot silently alter the reviewed restoration.
	write(t, filepath.Join(e.BackupRoot, backupID), "agents/general.yaml", "id: general\nname: Changed backup\n")
	if _, ok := restartEngine(t, next).Get(p.ID); ok {
		t.Fatal("modified backup was trusted")
	}
}

func TestPersistenceRewritesPermissiveSnapshotMode(t *testing.T) {
	e := persistentEngine(t)
	persistentPlan(t, e, "helper")
	if err := os.Chmod(e.PlansFile, 0o644); err != nil {
		t.Fatal(err)
	}
	restartEngine(t, e)
	info, err := os.Stat(e.PlansFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("restored mode: %v %v", info, err)
	}
}

func TestRollbackReferencesRejectMissingOrEditedRecords(t *testing.T) {
	for _, mode := range []string{"missing backup", "inline content", "traversal"} {
		t.Run(mode, func(t *testing.T) {
			e := persistentEngine(t)
			backupID := "catalog-pre-original-20261120T120000Z"
			backupDir := filepath.Join(e.BackupRoot, backupID)
			write(t, backupDir, "agents/general.yaml", "id: general\nname: Original\n")
			p, err := e.PlanRollback(context.Background(), backupID, Options{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(e.PlansFile)
			var snapshot planFile
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing backup":
				if err := os.Rename(backupDir, backupDir+"-moved"); err != nil {
					t.Fatal(err)
				}
			case "inline content":
				snapshot.Plans[0].Bundle.Files[0].Content = "injected literal"
			case "traversal":
				snapshot.Plans[0].RollbackOf = "../catalog-pre-other"
			}
			snapshot.Plans[0].Checksum = snapshot.Plans[0].checksum()
			raw, _ = json.Marshal(snapshot)
			if err := os.WriteFile(e.PlansFile, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok := restartEngine(t, e).Get(p.ID); ok {
				t.Fatal("unsafe rollback reference restored")
			}
		})
	}
}

func TestSecretRollbackRestartRequiresFreshApprovalThenApplies(t *testing.T) {
	e := persistentEngine(t)
	backupID := "catalog-pre-original-20261120T120000Z"
	const body = "id: general\napi_key: sk-abcdefghijklmnopqrstuvwxyz12345678901234567890\n"
	write(t, filepath.Join(e.BackupRoot, backupID), "agents/general.yaml", body)
	p, err := e.PlanRollback(context.Background(), backupID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	next := restartEngine(t, e)
	got, ok := next.Get(p.ID)
	if !ok {
		t.Fatal("rollback lost across restart")
	}
	if _, err := next.Apply(context.Background(), p.ID); err == nil {
		t.Fatal("rollback applied without fresh approval")
	}
	approve(t, next, got)
	if _, err := next.Apply(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if read(t, next.Root, "agents/general.yaml") != body {
		t.Fatal("approved backup content was not restored")
	}
}

func TestRollbackDeletionReferencesRejectChangedBackup(t *testing.T) {
	for _, mode := range []string{"file added", "symlink root", "file root"} {
		t.Run(mode, func(t *testing.T) {
			e := persistentEngine(t)
			backupID := "catalog-pre-deletion-20261120T120000Z"
			backupDir := filepath.Join(e.BackupRoot, backupID)
			if err := copyTree(e.Root, backupDir); err != nil {
				t.Fatal(err)
			}
			write(t, e.Root, "agents/extra.yaml", "id: extra\n")
			p, err := e.PlanRollback(context.Background(), backupID, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Files) != 1 || p.Files[0].Action != "delete" {
				t.Fatalf("expected deletion-only fixture: %+v", p.Files)
			}
			switch mode {
			case "file added":
				write(t, backupDir, "agents/extra.yaml", "id: extra\n")
			case "symlink root", "file root":
				moved := backupDir + "-moved"
				if err := os.Rename(backupDir, moved); err != nil {
					t.Fatal(err)
				}
				if mode == "symlink root" {
					if err := os.Symlink(moved, backupDir); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(backupDir, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := restartEngine(t, e).Get(p.ID); ok {
				t.Fatal("restored deletion from changed backup reference")
			}
		})
	}
}
