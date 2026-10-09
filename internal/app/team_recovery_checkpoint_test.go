//go:build !windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/checkpoint"
)

func TestRetainedCheckpointExcludesSharedProfileSiblingAndPreservesOwnLineage(t *testing.T) {
	r, input, actor := replacementStoreRig(t)
	ctx := context.Background()
	profile := input.Plan.LogicalAgentID
	for _, value := range []checkpoint.Checkpoint{
		{ID: "sibling", LogicalAgentID: profile, SourceSessionID: "another-enrolled-member", CreatedAt: "2099-01-01T00:00:00Z", Summary: "another member's task"},
		{ID: "unbound", LogicalAgentID: profile, CreatedAt: "2099-02-01T00:00:00Z", Summary: "unattributed legacy context"},
	} {
		if err := r.svc.Store.CreateCheckpoint(value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.svc.retainedRecoveryCheckpoint(ctx, input.Source.ID, profile, actor); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("shared profile substituted another checkpoint", err)
	}
	if err := r.svc.Store.CreateCheckpoint(checkpoint.Checkpoint{ID: "own", LogicalAgentID: profile, SourceSessionID: input.Source.ID, CreatedAt: "2026-10-09T00:00:00Z", Summary: "original member's task"}); err != nil {
		t.Fatal(err)
	}
	value, err := r.svc.retainedRecoveryCheckpoint(ctx, input.Source.ID, profile, actor)
	if err != nil || value.ID != "own" {
		t.Fatal("canonical source checkpoint missing", err)
	}
	if _, err := prepareReplacement(t, r, input); err != nil {
		t.Fatal(err)
	}
	value, err = r.svc.retainedRecoveryCheckpoint(ctx, input.DestinationID, profile, actor)
	if err != nil || value.ID != "own" {
		t.Fatal("same-actor replacement lost original checkpoint", err)
	}
	if err := r.svc.Store.CreateCheckpoint(checkpoint.Checkpoint{ID: "current", LogicalAgentID: profile, SourceSessionID: input.DestinationID, CreatedAt: "2026-10-10T00:00:00Z", Summary: "current execution's task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.retainedRecoveryCheckpoint(ctx, input.DestinationID, profile, "another-actor"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign actor traversed checkpoint lineage", err)
	}
}
