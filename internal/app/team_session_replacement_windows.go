//go:build windows

package app

import (
	"context"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func (*Service) replaceRetainedTeamExecution(context.Context, string, *store.SessionRow, *launch.Plan, string) error {
	return store.ErrSessionReplacementUnavailable
}
func (*Service) launchRetainedTeamReplacement(context.Context, string, string) error {
	return store.ErrSessionReplacementUnavailable
}
func (*Service) launchRetainedTeamReplacementLocked(context.Context, string, string) error {
	return store.ErrSessionReplacementUnavailable
}
