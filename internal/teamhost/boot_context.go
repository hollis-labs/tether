package teamhost

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/hollis-labs/libs/mesh/core/teams"
)

// InitialRunID resolves the future workflow ID for an eager member whose
// immutable mesh launch intent predates RunID propagation. It uses the durable
// intent lookup and the same derivation as LaunchWorkflow, which runs only after
// members are provisioned. Neither the launch record nor the request is changed.
func InitialRunID(ctx context.Context, db *sql.DB, req teams.ProvisionRequest) (string, error) {
	missing := func(err error) (string, error) {
		return "", fmt.Errorf("boot context: run id missing: %w: %w", ErrInvalidRequest, err)
	}
	if req.IdempotencyKey == "" || req.RunID != "" || req.Parent != "" {
		return missing(teams.ErrConflict)
	}
	var key string
	var payload []byte
	err := db.QueryRowContext(ctx, `SELECT l.launch_key,l.payload FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=?`, req.IdempotencyKey).Scan(&key, &payload)
	if err != nil {
		return missing(notFound(err))
	}
	var record teams.LaunchRecord
	if err := decode(payload, &record); err != nil {
		return missing(err)
	}
	runID := id("workflow", key)
	if key == "" || record.Key != key || (record.Run.ID != "" && record.Run.ID != runID) {
		return missing(teams.ErrConflict)
	}
	for _, intent := range record.Intents {
		if intent.Key != req.IdempotencyKey {
			continue
		}
		raw, err := encode(intent.Request)
		if err != nil {
			return missing(err)
		}
		matches, err := same(raw, req)
		if err != nil {
			return missing(err)
		}
		if matches {
			return runID, nil
		}
	}
	return missing(teams.ErrConflict)
}
