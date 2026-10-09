//go:build windows

package app

import (
	"context"

	"github.com/hollis-labs/tether/internal/store"
)

func (*Service) recoverRetainedCodexTeam(context.Context, string, *store.SessionRow, store.SessionShimRow) error {
	return store.ErrSessionReplacementUnavailable
}
