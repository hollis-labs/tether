//go:build !windows

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
)

func initializeShimCheckpoint(r shimhost.Receipt) error {
	path := filepath.Join(filepath.Dir(r.DescriptorPath), "bridge.json")
	state, err := shimbridge.ReadCheckpoint(path)
	if errors.Is(err, os.ErrNotExist) {
		state = shimbridge.Checkpoint{Session: r.Session, Instance: r.Instance, Generation: r.Generation, Journal: r.Journal}
		return shimhost.WritePrivateJSON(path, state)
	}
	if err != nil {
		return &shimhost.Failure{Code: "outcome_unknown", Message: "bridge checkpoint unavailable"}
	}
	if state.Session != r.Session || state.Instance != r.Instance || state.Generation != r.Generation {
		return &shimhost.Failure{Code: "identity_mismatch", Message: "bridge checkpoint identity differs"}
	}
	if state.Journal != "" && r.Journal != "" && state.Journal != r.Journal {
		return &shimhost.Failure{Code: "journal_mismatch", Message: "bridge checkpoint journal differs"}
	}
	if r.Journal != "" && state.Journal == "" {
		if state.Counter != 0 || state.Cursor != "" || len(state.Partial) > 0 || len(state.Init) > 0 || state.Exit != nil {
			return &shimhost.Failure{Code: "journal_mismatch", Message: "used checkpoint has no journal witness"}
		}
		state.Journal = r.Journal
		return shimhost.WritePrivateJSON(path, state)
	}
	return nil
}

func (s *Service) waitShimBinding(ctx context.Context, id string) error {
	if value, ok := s.shimBindingWait.Load(id); ok {
		select {
		case <-value.(chan struct{}):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
