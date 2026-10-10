package app

import (
	"context"
	"io"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
)

// AttachSessionWithSnapshot exposes the actual retained byte window at atomic
// subscription admission. The callback precedes every replay/live byte write.
func (s *Service) AttachSessionWithSnapshot(ctx context.Context, id string, w io.Writer, since int64, onSnapshot func(agentsessions.AttachSnapshot) error) error {
	return s.Manager.AttachWith(ctx, id, w, agentsessions.AttachOptions{SinceSeq: since, OnSnapshot: onSnapshot})
}
