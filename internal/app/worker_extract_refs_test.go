package app

import (
	"context"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestWorkerLaunchExtractRefsRefusesProviderWithoutRelay(t *testing.T) {
	r := newIdentityRig(t, workerwire.FeatureSessionCred, workerwire.FeatureExtractRefs)
	plan := &launch.Plan{ProviderBrand: "codex", ExtractRefs: true}
	row := store.SessionRow{ID: "codex-extract", WorkerID: r.worker, State: "created", Workspace: t.TempDir()}
	if err := r.db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	_, err := r.svc.launchWorkerSession(context.Background(), &row, plan)
	if err == nil || !strings.Contains(err.Error(), "claude MCP relay") {
		t.Fatalf("err = %v", err)
	}
	saved, err := r.db.GetSession(row.ID)
	if err != nil || saved.State != "failed" || saved.RefAttribution.Valid {
		t.Fatalf("failed row=%+v err=%v", saved, err)
	}
	select {
	case <-r.specs:
		t.Fatal("unsupported launch sent")
	default:
	}
}

func TestWorkerLaunchExtractRefsCapabilityAndAttribution(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled         bool
		features        []string
		wantError       string
		wantAttribution string
	}{
		{"enabled", true, []string{workerwire.FeatureSessionCred, workerwire.FeatureExtractRefs}, "", store.RefAttributionProxy},
		{"disabled", false, []string{workerwire.FeatureSessionCred, workerwire.FeatureExtractRefs}, "", store.RefAttributionNone},
		{"old worker enabled", true, []string{workerwire.FeatureSessionCred}, "upgrade tether-worker", ""},
		{"old worker default", false, []string{workerwire.FeatureSessionCred}, "", ""},
		{"no credential", true, []string{workerwire.FeatureExtractRefs}, "requires a worker session credential", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newIdentityRig(t, tc.features...)
			r.svc.Catalog.Global.Catalog.Defaults.ExtractRefs = tc.enabled
			spec, err := r.launch("worker-extract", "")
			row, rowErr := r.db.GetSession("worker-extract")
			if rowErr != nil {
				t.Fatal(rowErr)
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v", err)
				}
				if row.State != "failed" || row.RefAttribution.Valid {
					t.Fatalf("failed launch: %+v", row)
				}
				select {
				case <-r.specs:
					t.Fatal("unsupported launch sent to worker")
				default:
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if spec.ExtractRefs != tc.enabled || spec.Session != row.ID {
				t.Fatal("worker received wrong policy/session")
			}
			if row.RefAttribution.String != tc.wantAttribution || row.RefAttribution.Valid != (tc.wantAttribution != "") {
				t.Fatalf("attribution=%v", row.RefAttribution)
			}
		})
	}
}
