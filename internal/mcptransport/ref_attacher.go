package mcptransport

import (
	"context"
	"fmt"

	"github.com/hollis-labs/tether/internal/store"
)

// hubRefAttacher is installed only on a verified session's hub-owned view.
// It records traffic relayed by that view, not assertions received over HTTP.
type hubRefAttacher struct {
	sessionID string
	store     interface {
		AttachSessionRef(store.SessionRefRow) (store.AttachRefResult, error)
	}
}

func (r hubRefAttacher) AttachSessionRef(ctx context.Context, sessionID, kind, refID, uri, relation, _ string, parentItemID string) error {
	if r.sessionID == "" || sessionID != r.sessionID {
		return fmt.Errorf("hub ref capture: session binding mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := r.store.AttachSessionRef(store.SessionRefRow{SessionID: r.sessionID, Kind: kind, RefID: refID, URI: uri, Relation: relation, Source: store.SourceProxy, ParentItemID: parentItemID})
	return err
}
