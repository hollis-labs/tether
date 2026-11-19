package catalogapply

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/operatorattest"
)

const attestationGlobal = "version: 1\noperator_attestation:\n  issuer: tangent\n  max_age_seconds: 300\n  keys:\n    - id: tangent-1\n      public_key: ed25519:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"

// CW-20261114-0152: a plan that adds, changes or removes global.yaml's
// operator_attestation is privileged, and only the operator approves it, even
// where delegated privileged approval is on.
func TestOperatorAttestationPlanIsOperatorOnly(t *testing.T) {
	e := newEngine(t)
	e.DelegatedApps, e.DelegatedPrivileged = []string{"tachyon"}, true
	ctx := context.Background()
	p, err := e.PlanBundle(ctx, Bundle{Name: "trust", Files: []File{{Path: "global.yaml", Content: attestationGlobal}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.OperatorOnly || !slices.Contains(p.Privileged, "global.yaml: "+ReasonOperatorAttestation) {
		t.Fatalf("plan %+v", p)
	}
	if _, err = e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Delegated: true, AckPrivileged: true}); err == nil || !strings.Contains(err.Error(), "only the operator credential") {
		t.Fatalf("delegated approval of the trust root: %v", err)
	}
	approve(t, e, p)

	// Another global.yaml edit that leaves the block alone is not operator-only.
	other, err := e.PlanBundle(ctx, Bundle{Name: "other", Files: []File{{Path: "global.yaml", Content: "version: 2\n"}}}, Options{})
	if err != nil || other.OperatorOnly {
		t.Fatalf("a global.yaml edit without the block: %+v %v", other, err)
	}
}

// Applying it records the block as approved before the reload, and a reload
// that refuses it puts the previous record back.
func TestOperatorAttestationApplyRecordsAndRestores(t *testing.T) {
	for _, reloadFails := range []bool{false, true} {
		e := newEngine(t)
		var got *operatorattest.RawConfig
		var recorded, restored bool
		e.OnOperatorAttestation = func(_ context.Context, _ string, next *operatorattest.RawConfig) (func(), error) {
			recorded, got = true, next
			return func() { restored = true }, nil
		}
		e.Reload = func(context.Context) ([]string, error) {
			if !recorded {
				t.Error("reload ran before the block was recorded as approved")
			}
			if reloadFails {
				return nil, errors.New("refused")
			}
			return nil, nil
		}
		ctx := context.Background()
		p, err := e.PlanBundle(ctx, Bundle{Name: "trust", Files: []File{{Path: "global.yaml", Content: attestationGlobal}}}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		approve(t, e, p)
		_, err = e.Apply(ctx, p.ID)
		if got == nil || got.Issuer != "tangent" || len(got.Keys) != 1 {
			t.Fatalf("recorded block %+v", got)
		}
		if reloadFails != (err != nil) || restored != reloadFails {
			t.Fatalf("reload fails %v: apply err %v, restored %v", reloadFails, err, restored)
		}
	}
}
