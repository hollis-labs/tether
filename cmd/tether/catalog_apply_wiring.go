package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/catalogcheck"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

// catalogApplyPaths returns where the approver's passphrase hash and the
// catalog backups live: beside the state database, so they follow the
// catalog's own defaults.state_db.
func catalogApplyPaths(cat *config.Catalog) (approverFile, backupRoot string) {
	stateDir := filepath.Dir(config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths))
	return filepath.Join(stateDir, "catalog-approver.json"), filepath.Join(stateDir, "catalog-backups")
}

// newCatalogApplyEngine builds the plan/approve/apply engine over the daemon's
// catalog root (CW-20261012-0124).
func newCatalogApplyEngine(svc *app.Service, issuers ...*authissuer.Issuer) *catalogapply.Engine {
	approverFile, backupRoot := catalogApplyPaths(svc.Catalog)
	engine := &catalogapply.Engine{
		Root:        svc.CatalogRoot,
		ApprovalOff: strings.EqualFold(strings.TrimSpace(os.Getenv("TETHER_CATALOG_APPROVAL")), "off"),
		Preflight: func(root string) []catalogcheck.Result {
			cat, err := config.LoadLayered(root)
			if err != nil {
				return nil
			} // The structural report already explains parse failures.
			impacts, err := svc.MCPGrantImpacts(context.Background(), cat)
			if err != nil {
				return []catalogcheck.Result{catalogcheck.Fail("running session MCP grants", err.Error(), "Retry validation when the session store is available")}
			}
			var results []catalogcheck.Result
			for _, impact := range impacts {
				for _, issue := range impact.Servers {
					result := catalogcheck.Warn("running session MCP grants", fmt.Sprintf("session %q server %q: %s; effective grant will narrow", impact.SessionID, issue.Server, issue.Reason), impact.Hint)
					result.Entity = "session:" + impact.SessionID
					results = append(results, result)
				}
			}
			return results
		},
		BackupRoot:     backupRoot,
		PlansFile:      filepath.Join(filepath.Dir(approverFile), "catalog-plans.json"),
		PolicyFile:     filepath.Join(filepath.Dir(approverFile), "catalog-approvers.json"),
		BreakglassFile: filepath.Join(filepath.Dir(approverFile), "catalog-breakglass.json"),
		Approver:       catalogapply.NewApprover(approverFile),
		TTL:            catalogPlanTTL(os.Getenv("TETHER_CATALOG_PLAN_TTL")),
		// Tachyon is the operator's own app (CW-20261030-0190): it approves non-privileged
		// plans by delegation. TETHER_CATALOG_DELEGATED_PRIVILEGED=1 widens that to every plan.
		DelegatedApps:       []string{"tachyon"},
		DelegatedPrivileged: os.Getenv("TETHER_CATALOG_DELEGATED_PRIVILEGED") == "1",
		// The operator attestation trust root is recorded as approved while its
		// plan applies (CW-20261114-0152).
		OnOperatorAttestation: svc.RecordOperatorAttestation,
		Reload: func(ctx context.Context) ([]string, error) {
			rep, err := svc.ReloadCatalog(ctx)
			if err != nil {
				return nil, err
			}
			return rep.RestartRequired, nil
		},
		RunningSessions: func(_ context.Context, touches []string) ([]string, error) {
			rows, err := svc.Store.ListSessions(store.ListSessionsOptions{State: "running", Limit: 1000})
			if err != nil {
				return nil, err
			}
			want := map[string]bool{}
			for _, t := range touches {
				want[t] = true
			}
			var ids []string
			for _, r := range rows {
				if want["agent:"+r.LogicalAgentID] || want["launch:"+r.LaunchID] {
					ids = append(ids, r.ID)
				}
			}
			return ids, nil
		},
	}
	if engine.ApprovalOff {
		slog.Warn("catalog approval: off; temporary development window", "approved_by", "approval-disabled")
	} else {
		slog.Info("catalog approval: on")
	}
	if len(issuers) > 0 && issuers[0] != nil {
		engine.WindowTrust = issuers[0].Keys
	}
	if err := engine.RestorePlans(context.Background()); err != nil {
		slog.Error("catalog plan persistence reload failed; saved plans unavailable", "error", err)
	}
	if _, err := engine.Approvers(); err != nil {
		slog.Error("catalog approver policy/window unavailable; delegation fails closed", "error", err)
	}
	return engine
}

// catalogPlanTTL parses TETHER_CATALOG_PLAN_TTL (a Go duration such as 4h) so a
// dev window can keep catalog plans approvable longer than the 15 minute
// default. Empty, invalid or out-of-range (under 1m or over 168h) values keep the
// default; approval itself is unchanged and still needs the operator passphrase.
func catalogPlanTTL(raw string) time.Duration {
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d < time.Minute || d > 7*24*time.Hour {
		slog.Warn("invalid TETHER_CATALOG_PLAN_TTL; falling back to 15m (valid range 1m-168h)")
		return 0
	}
	return d
}
