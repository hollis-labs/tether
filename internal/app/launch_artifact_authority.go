package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/google/uuid"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// launchArtifactAdmission narrows the existing accepted, locked launch to one
// inactive artifact operation. This is neither a principal nor a new launch
// permission. Canonical session and plan predicates are checked again by the
// harness under its complete artifact locks.
func (s *Service) launchArtifactAdmission(ctx context.Context, id string, row *store.SessionRow, plan *launch.Plan) (launchartifacts.Admission, error) {
	if s.Store == nil || row == nil || row.ID != id || row.State != string(session.StateCreated) || (row.PID.Valid && row.PID.Int64 != 0) {
		return launchartifacts.Admission{}, fmt.Errorf("artifact admission: canonical created session required")
	}
	acceptedRow := *row
	s.launchMu.Lock()
	gate := s.launches[id]
	var holder string
	if gate != nil {
		holder = gate.holder
	}
	held := holder != "" && len(gate.slot) == 1
	s.launchMu.Unlock()
	if !held {
		return launchartifacts.Admission{}, fmt.Errorf("artifact admission: launch gate not held")
	}
	version, err := artifactPlanVersion(plan)
	if err != nil {
		return launchartifacts.Admission{}, err
	}
	// Use the actually opened durable store, not a guessed catalog default.
	var databasePath string
	if err := s.Store.DB().QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&databasePath); err != nil {
		return launchartifacts.Admission{}, err
	}
	if !filepath.IsAbs(databasePath) {
		return launchartifacts.Admission{}, fmt.Errorf("artifact admission: durable local database required")
	}
	operationContext := ctx
	validate := func(ctx context.Context) error {
		if err := operationContext.Err(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.launchMu.Lock()
		held := s.launches[id] == gate && gate.holder == holder && len(gate.slot) == 1
		s.launchMu.Unlock()
		if !held {
			return fmt.Errorf("artifact admission: launch gate custody changed")
		}
		current, err := s.Store.GetSession(id)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(*current, acceptedRow) {
			return fmt.Errorf("artifact admission: canonical session changed")
		}
		stored, err := s.Store.GetLaunchPlan(id)
		if err != nil {
			return err
		}
		storedVersion, err := artifactPlanVersion(stored)
		if err != nil || storedVersion != version {
			return fmt.Errorf("artifact admission: canonical launch plan changed")
		}
		acceptedVersion, err := artifactPlanVersion(plan)
		if err != nil || acceptedVersion != version {
			return fmt.Errorf("artifact admission: accepted launch plan changed")
		}
		return nil
	}
	return launchartifacts.Admission{OperationID: uuid.NewString(), DecisionID: "tether.session-launch:" + id,
		Version: version, Owner: fmt.Sprintf("tether-daemon-uid:%d", os.Geteuid()), ControlParent: filepath.Dir(databasePath),
		LocalFilesystem: true, Validate: validate}, nil
}

func artifactPlanVersion(plan *launch.Plan) (string, error) {
	if plan == nil {
		return "", fmt.Errorf("artifact admission: accepted launch plan required")
	}
	accepted := *plan
	// Compile records derived provenance in Shared; it does not change the
	// canonical accepted launch decision or confer additional authority.
	accepted.Shared = nil
	return launchartifacts.Digest(accepted)
}
