package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/workspace"
)

// Native state belongs to a canonical session, not to the ambient operator
// home. Fresh planting still owns config, auth and the loopback endpoint.
func (s *Service) prepareRecoveryNativeState(ctx context.Context, plan *launch.Plan, target string) error {
	if plan.NativeResumeOnly {
		return nativeOnlyContext(plan)
	}
	if plan.ResumeSourceSessionID != "" && plan.ResumeProviderSessionID != "" && plan.ProviderBrand == "codex" {
		source, err := s.Store.GetLaunchPlan(plan.ResumeSourceSessionID)
		if err != nil {
			return err
		}
		if source.ProviderID != plan.ProviderID || source.ProviderBrand != plan.ProviderBrand {
			return fmt.Errorf("native state: source provider mismatch")
		}
		root := source.NativeStateRoot
		if root == "" {
			root, err = s.Store.SessionBootDir(ctx, plan.ResumeSourceSessionID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if root != "" {
			// Only absence before copying is a recoverable native-history loss.
			// Missing files during a partial copy and unsafe roots abort launch.
			if err := workspace.CopyNativeState(root, target, plan.ProviderBrand); err != nil && !errors.Is(err, workspace.ErrNativeStateMissing) {
				return err
			}
		}
	}
	plan.NativeStateRoot = target
	return nil
}
