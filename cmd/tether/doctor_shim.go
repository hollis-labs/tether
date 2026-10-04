package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

// These checks read configuration and persisted symptoms only: no control
// connection, placement, takeover, signaling, or migration is performed.
func checkShimHosting(cat *config.Catalog) []checkResult {
	service := &app.Service{Catalog: cat}
	host := service.LaunchHost()
	results := []checkResult{ok("launch-host", fmt.Sprintf("%s; shim is opt-in for Claude streaming-stdio; production hosting requires a planned systemd-user activation", host))}
	if cat == nil {
		return results
	}
	db, err := store.OpenReadOnly(config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths))
	if err != nil {
		return results
	}
	defer func() { _ = db.Close() }()
	rows, err := db.ListSessionShims(context.Background())
	if err != nil {
		return results
	}
	for _, row := range rows {
		session, err := db.GetSession(row.SessionID)
		if err != nil || session.State != "detached" {
			continue
		}
		reason := "outcome_unknown"
		if events, err := db.QueryEvents(store.EventFilter{SessionID: row.SessionID, Kinds: []string{"session.shim_status"}, Limit: 1}); err == nil && len(events) == 1 {
			var status struct {
				Reason string `json:"reason"`
			}
			if json.Unmarshal([]byte(events[0].PayloadJSON), &status) == nil && status.Reason != "" {
				reason = status.Reason
			}
		}
		results = append(results, warn("shim-detached", fmt.Sprintf("session=%s reason=%s key=%s backend=%s unit=%s socket=%s", row.SessionID, reason, row.ShimKey, row.HostBackend, row.UnitName, row.SocketPath), "restore the private receipt or host connectivity and retry reconciliation; unknown outcomes retain the child and authority; no explicit retirement API exists"))
	}
	return results
}
