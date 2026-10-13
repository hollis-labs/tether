package main

import (
	"context"
	"path/filepath"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/catalogapply"
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
func newCatalogApplyEngine(svc *app.Service) *catalogapply.Engine {
	approverFile, backupRoot := catalogApplyPaths(svc.Catalog)
	return &catalogapply.Engine{
		Root:       svc.CatalogRoot,
		BackupRoot: backupRoot,
		Approver:   catalogapply.NewApprover(approverFile),
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
}
