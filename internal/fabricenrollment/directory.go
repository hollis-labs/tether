package fabricenrollment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

// DirectoryRecord is a private host projection, not a second shared wire type.
// It contains no owner, source location, instructions, policy or runtime locator.
// Capability IDs are offered services filtered by explicit host publication.
type DirectoryRecord struct {
	URN            mesh.URN                 `json:"urn"`
	Kind           mesh.ActorKind           `json:"kind"`
	Lifecycle      mesh.EnrollmentLifecycle `json:"lifecycle"`
	Definition     *mesh.DefinitionRef      `json:"definition,omitempty"`
	Capabilities   []string                 `json:"capabilities"`
	RecordRevision string                   `json:"record_revision"`
	VerifiedAt     time.Time                `json:"verified_at"`
	ValidUntil     time.Time                `json:"valid_until"`
}
type DirectoryPage struct {
	Records []DirectoryRecord
	// Cursor is private owner-scope pagination state, not a publication record.
	Cursor mesh.URN
}

// Directory authorizes the complete owner-scoped identity index first. A row's
// visibility and services additionally require explicit publication policy.
// The cursor may cover unpublished identities within that authorized scope;
// a future public route must not expose this host pagination state as metadata.
func (s *Service) Directory(ctx context.Context, caller, owner, after mesh.URN, limit int) (DirectoryPage, error) {
	result := DirectoryPage{Records: []DirectoryRecord{}}
	if err := s.allow(ctx, caller, owner, owner, ReadDirectory); err != nil {
		return result, err
	}
	records, err := s.repo.Enrollments(ctx, owner, after, limit)
	if err != nil {
		return result, err
	}
	for _, record := range records {
		result.Cursor = record.Actor.Value.URN
		if record.Actor.Value.Lifecycle != mesh.EnrollmentActive {
			continue
		}
		auth := Authorization{caller, owner, record.Actor.Value.URN, ReadDirectory}
		if err := s.allow(ctx, caller, owner, record.Actor.Value.URN, ReadDirectory); err != nil {
			if errors.Is(err, ErrDenied) {
				continue
			}
			return DirectoryPage{}, err
		}
		var verified definitionresolve.VerifiedDefinition
		if record.Agent != nil {
			verified, err = s.definitions.Load(ctx, record.Agent.Value.Definition)
			if err != nil {
				return DirectoryPage{}, err
			}
			if verified.Pin != record.Agent.Value.Definition || verified.Definition == nil {
				return DirectoryPage{}, definitionresolve.ErrPinMismatch
			}
		}
		publication, err := s.advertise(ctx, auth, verified)
		if err != nil {
			return DirectoryPage{}, err
		}
		if !publication.Publish {
			continue
		}
		ids := append([]string{}, publication.Capabilities...)
		offered := map[string]bool{}
		if verified.Definition != nil {
			for _, capability := range verified.Definition.Capabilities {
				offered[capability.ID] = true
			}
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !offered[id] || seen[id] {
				return DirectoryPage{}, fabricstore.ErrInvalid
			}
			seen[id] = true
		}
		sort.Strings(ids)
		projection := DirectoryRecord{URN: record.Actor.Value.URN, Kind: record.Actor.Value.Kind, Lifecycle: record.Actor.Value.Lifecycle, Capabilities: ids, RecordRevision: fmt.Sprintf("%d", record.Actor.Version)}
		if record.Agent != nil {
			pin := record.Agent.Value.Definition
			projection.Definition = &pin
			projection.RecordRevision += fmt.Sprintf("/%d", record.Agent.Version)
		}
		result.Records = append(result.Records, projection)
	}
	current, err := s.repo.Enrollments(ctx, owner, after, limit)
	if err != nil {
		return DirectoryPage{}, err
	}
	if !reflect.DeepEqual(records, current) {
		return DirectoryPage{}, fabricstore.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return DirectoryPage{}, err
	}
	observed := s.now().UTC()
	if observed.IsZero() {
		return DirectoryPage{}, fabricstore.ErrInvalid
	}
	for i := range result.Records {
		result.Records[i].VerifiedAt = observed
		result.Records[i].ValidUntil = observed.Add(s.directoryTTL)
	}
	return result, nil
}
