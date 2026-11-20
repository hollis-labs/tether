package catalogapply

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/operatorattest"
	"os"
	"path/filepath"
	"testing"
)

func TestApprovalOffAutoapprovesAndPreservesIntegrity(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t)
	e.ApprovalOff = true
	e.Approver = nil
	e.RunningSessions = func(context.Context, []string) ([]string, error) { return []string{"running"}, nil }
	bundle := Bundle{Name: "dev-window", Files: []File{{Path: "agents/general.yaml", Content: "id: general\nname: Changed\npermissions:\n  network: true\n"}}}
	p, err := e.PlanBundle(ctx, bundle, Options{ProtectedPaths: []string{"agents/general.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		approved, approveErr := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Delegated: true})
		if approveErr != nil || approved.ApprovedBy != "approval-disabled" {
			t.Fatalf("idempotent approval: %+v %v", approved, approveErr)
		}
	}
	if p.State != StateApproved || p.ApprovedBy != "approval-disabled" || !p.AllowRunning || p.ApprovedAt == nil {
		t.Fatalf("not autoapproved: %+v", p)
	}
	if _, err = e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Delegated: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Approve(ApprovalRequest{PlanID: p.ID, Digest: "wrong"}); err == nil {
		t.Fatal("wrong digest approved")
	}
	res, err := e.Apply(ctx, p.ID)
	if err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	history, err := e.History(1)
	if err != nil || len(history) != 1 || history[0].ApprovedBy != "approval-disabled" {
		t.Fatalf("audit: %+v %v", history, err)
	}
	p, err = e.PlanBundle(ctx, Bundle{Name: "stale", Files: []File{{Path: "agents/general.yaml", Content: "id: general\nname: Next\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.Root, "agents/general.yaml"), []byte("id: general\nname: Other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Apply(ctx, p.ID); err == nil {
		t.Fatal("stale base applied")
	}
}

func TestApprovalModeDefaultsAndInvalidPlans(t *testing.T) {
	for _, off := range []bool{false, true} {
		t.Run(map[bool]string{false: "on", true: "off"}[off], func(t *testing.T) {
			e := newEngine(t)
			e.ApprovalOff = off
			v, err := e.Approvers()
			if err != nil || v.Approval != map[bool]string{false: "on", true: "off"}[off] {
				t.Fatalf("view: %+v %v", v, err)
			}
			b := Bundle{Name: "mode", Files: []File{{Path: "agents/general.yaml", Content: "id: general\nname: New\n"}}}
			p, err := e.PlanBundle(context.Background(), b, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if !off && p.State != StatePlanned {
				t.Fatal(p.State)
			}
			if !off {
				if _, err = e.Apply(context.Background(), p.ID); err == nil {
					t.Fatal("unapproved apply")
				}
			}
			invalid, err := e.PlanBundle(context.Background(), Bundle{Name: "invalid", Files: []File{{Path: "agents/general.yaml", Content: "INVALID"}}}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if invalid.State != StateInvalid || invalid.ApprovedAt != nil {
				t.Fatalf("invalid approved: %+v", invalid)
			}
			if _, err = e.Apply(context.Background(), invalid.ID); err == nil {
				t.Fatal("invalid applied")
			}
		})
	}
}

func TestApprovalOffOperatorOnlyAndRollback(t *testing.T) {
	e := newEngine(t)
	e.ApprovalOff = true
	e.Approver = nil
	// A malformed delegation policy must not impose an approval fence in off mode.
	e.PolicyFile = filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(e.PolicyFile, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := e.Approvers()
	if err != nil || v.Approval != "off" {
		t.Fatalf("view %+v %v", v, err)
	}
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "trust", Files: []File{{Path: "global.yaml", Content: "version: 1\noperator_attestation:\n  operator: dev\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.OperatorOnly || p.State != StateApproved {
		t.Fatalf("operator fence not lifted: %+v", p)
	}
	recorded := false
	e.OnOperatorAttestation = func(context.Context, string, *operatorattest.RawConfig) (func(), error) {
		recorded = true
		return func() { recorded = false }, nil
	}
	e.Reload = func(context.Context) ([]string, error) { return nil, errors.New("reload refused") }
	result, err := e.Apply(context.Background(), p.ID)
	if err == nil || result == nil || !result.RolledBack || recorded {
		t.Fatalf("rollback: %+v %v recorded=%v", result, err, recorded)
	}
	body, err := os.ReadFile(filepath.Join(e.Root, "global.yaml"))
	if err != nil || string(body) != "version: 1\n" {
		t.Fatalf("restored %q %v", body, err)
	}
}
