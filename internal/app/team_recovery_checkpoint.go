package app

import (
	"context"

	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/teamhost"
)

// Pool members can share a catalog logical-agent profile. That profile is not
// checkpoint ownership. Read only the canonical source and its immutable
// replacement ancestors under the same original actor; never a sibling member.
func (s *Service) retainedRecoveryCheckpoint(ctx context.Context, source, profile, actor string) (*checkpoint.Checkpoint, error) {
	var id, selectedSource string
	err := s.Store.DB().QueryRowContext(ctx, `WITH RECURSIVE own_sources(id) AS (
	 SELECT ? WHERE EXISTS(SELECT 1 FROM runtime_bindings WHERE session_id=? AND target_urn=?)
	 OR EXISTS(SELECT 1 FROM session_replacements WHERE (source_session_id=? OR replacement_session_id=?) AND actor_uri=?)
	 UNION
 SELECT x.source_session_id FROM session_replacements x JOIN own_sources o ON x.replacement_session_id=o.id WHERE x.actor_uri=?
 ) SELECT id,source_session_id FROM checkpoints WHERE logical_agent_id=? AND source_session_id IN (SELECT id FROM own_sources) ORDER BY created_at DESC,id DESC LIMIT 1`, source, source, actor, source, source, actor, actor, profile).Scan(&id, &selectedSource)
	if err != nil {
		return nil, err
	}
	value, err := s.Store.GetCheckpoint(id)
	if err != nil {
		return nil, err
	}
	if value.SourceSessionID != selectedSource || value.LogicalAgentID != profile {
		return nil, teamhost.ErrSessionUnavailable
	}
	return value, ctx.Err()
}
