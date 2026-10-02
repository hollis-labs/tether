package mcptransport

import (
	"context"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

type daemonRefAttacher struct{ client *client.Client }

func (r daemonRefAttacher) AttachSessionRef(ctx context.Context, sessionID, kind, refID, uri, relation, source, parentItemID string) error {
	_, err := r.client.AttachSessionRef(ctx, sessionID, api.SessionRefAttachRequest{Kind: kind, RefID: refID, URI: uri, Relation: relation, Source: source, ParentItemID: parentItemID})
	return err
}
