package catalogapply

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestApproveBatchHappyPathAndOneLockoutAttempt(t *testing.T) {
	e := persistentEngine(t)
	p := persistentPlan(t, e, "one")
	q := persistentPlan(t, e, "two")
	req := BatchApprovalRequest{Plans: []ApprovalItem{{ID: p.ID, Digest: p.Digest}, {ID: q.ID, Digest: q.Digest}}, Passphrase: "wrong"}
	if _, err := e.ApproveBatch(req); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if e.Approver.failures != 1 {
		t.Fatalf("one batch consumed %d failures", e.Approver.failures)
	}
	for _, p := range []*Plan{p, q} {
		got, _ := e.Get(p.ID)
		if got.State != StatePlanned {
			t.Fatal("wrong passphrase approved plan")
		}
	}
	req.Passphrase = passphrase
	results, err := e.ApproveBatch(req)
	if err != nil || len(results) != 2 {
		t.Fatalf("batch: %+v %v", results, err)
	}
	for _, r := range results {
		if r.Error != "" || r.Plan.State != StateApproved || r.Plan.ApprovedAt == nil {
			t.Fatalf("result %+v", r)
		}
	}
	next := restartEngine(t, e)
	if len(next.plans) != 0 {
		t.Fatal("approved batch persisted")
	}
}

func TestApproveBatchPerPlanFailuresPreserveAllChecks(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	e.Now = func() time.Time { return now }
	good := persistentPlan(t, e, "good")
	stale := persistentPlan(t, e, "stale")
	write(t, e.Root, "agents/stale.yaml", "id: stale\nname: changed\n")
	invalid, err := e.PlanBundle(context.Background(), Bundle{Name: "invalid", Files: []File{{Path: "agents/invalid.yaml", Content: "id: INVALID\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	expired := persistentPlan(t, e, "expired")
	e.plans[expired.ID].ExpiresAt = now.Add(-time.Second)
	privileged, err := e.PlanBundle(context.Background(), Bundle{Name: "priv", Files: []File{{Path: "agents/priv.yaml", Content: "id: priv\npermissions: [all]\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	running := persistentPlan(t, e, "running")
	e.plans[running.ID].RunningSessions = []string{"session"}
	wrong := persistentPlan(t, e, "wrong")
	items := []ApprovalItem{{ID: good.ID, Digest: good.Digest}, {ID: stale.ID, Digest: stale.Digest}, {ID: invalid.ID, Digest: invalid.Digest}, {ID: expired.ID, Digest: expired.Digest}, {ID: privileged.ID, Digest: privileged.Digest}, {ID: running.ID, Digest: running.Digest}, {ID: wrong.ID, Digest: "mismatch"}, {ID: "missing", Digest: good.Digest}}
	results, err := e.ApproveBatch(BatchApprovalRequest{Plans: items, Passphrase: passphrase})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range results {
		if i == 0 {
			if r.Plan == nil || r.Plan.State != StateApproved {
				t.Fatalf("good: %+v", r)
			}
			continue
		}
		if r.Error == "" || r.Plan != nil {
			t.Fatalf("failure approved: %+v", r)
		}
		if p, ok := e.Get(r.ID); ok && (p.ApprovedAt != nil || p.State == StateApproved) {
			t.Fatalf("failure mutated: %+v", p)
		}
	}
	// Privileged and running flags remain usable, not silently disabled.
	results, err = e.ApproveBatch(BatchApprovalRequest{Plans: []ApprovalItem{{ID: privileged.ID, Digest: privileged.Digest}, {ID: running.ID, Digest: running.Digest}}, Passphrase: passphrase, AckPrivileged: true, AllowRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Error != "" || !r.Plan.AllowRunning {
			t.Fatalf("explicit flags failed: %+v", r)
		}
	}
}

func TestApproveBatchDelegationCannotApproveTrustRoot(t *testing.T) {
	e := newEngine(t)
	e.DelegatedPrivileged = true
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "trust", Files: []File{{Path: "global.yaml", Content: "version: 1\noperator_attestation:\n  key_id: next\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	results, err := e.ApproveBatch(BatchApprovalRequest{Plans: []ApprovalItem{{ID: p.ID, Digest: p.Digest}}, Delegated: true, AckPrivileged: true})
	if err != nil || len(results) != 1 || results[0].Error == "" {
		t.Fatalf("trust-root delegation: %+v %v", results, err)
	}
}

func TestBatchValidationAndLockout(t *testing.T) {
	e := newEngine(t)
	p := persistentPlan(t, e, "helper")
	item := ApprovalItem{ID: p.ID, Digest: p.Digest}
	for _, items := range [][]ApprovalItem{nil, {item, item}, {{ID: ""}}, make([]ApprovalItem, maxPlans+1)} {
		if _, err := e.ApproveBatch(BatchApprovalRequest{Plans: items, Passphrase: passphrase}); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	for i := 0; i < maxFailures; i++ {
		if _, err := e.ApproveBatch(BatchApprovalRequest{Plans: []ApprovalItem{item}, Passphrase: "wrong"}); !errors.Is(err, ErrBadPassphrase) {
			t.Fatal(err)
		}
	}
	if _, err := e.ApproveBatch(BatchApprovalRequest{Plans: []ApprovalItem{item}, Passphrase: passphrase}); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("locked batch: %v", err)
	}
	// Missing credentials fail closed.
	e.Approver = NewApprover(filepath.Join(t.TempDir(), "absent"))
	if _, err := e.ApproveBatch(BatchApprovalRequest{Plans: []ApprovalItem{item}, Passphrase: passphrase}); !errors.Is(err, ErrNoPassphrase) {
		t.Fatal(err)
	}
}
