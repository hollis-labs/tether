//go:build windows

package app

import (
	"context"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

type shimHosting struct{}

func (s *Service) waitShimBinding(context.Context, string) error { return nil }

func (s *Service) shimHealth(string) *api.ShimHealthStatus { return nil }

func (s *Service) settleShimBridgeExit(string) {}

func (s *Service) prepareShimStart(_ context.Context, _ *launch.Plan, req agentsessions.StartRequest) (agentsessions.StartRequest, error) {
	return req, nil
}
func (s *Service) stopShimSession(string) (bool, error)                 { return false, nil }
func (s *Service) reconcileShim(store.StaleSession) bool                { return false }
func (s *Service) shimSessionRetained(string) bool                      { return false }
func (s *Service) detachShimSession(context.Context, string) bool       { return false }
func shimBridgeTerminal(*store.Store, string, agentsessions.State) bool { return false }
