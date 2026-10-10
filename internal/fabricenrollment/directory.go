package fabricenrollment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"reflect"
	"sort"
	"syscall"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

// DirectoryRecord is a private host projection, not a second shared wire type.
// It contains no owner, source location, instructions, policy or runtime locator.
// Capability IDs are offered services filtered by explicit host publication.
type DirectoryRecord struct {
	Authority      fabricstore.EnrollmentAuthority `json:"authority"`
	URN            mesh.URN                        `json:"urn"`
	Kind           mesh.ActorKind                  `json:"kind"`
	Lifecycle      mesh.EnrollmentLifecycle        `json:"lifecycle"`
	Definition     *mesh.DefinitionRef             `json:"definition,omitempty"`
	Capabilities   []string                        `json:"capabilities"`
	RecordRevision string                          `json:"record_revision"`
	VerifiedAt     time.Time                       `json:"verified_at"`
	ValidUntil     time.Time                       `json:"valid_until"`
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
	if err := s.allow(ctx, Authorization{Caller: caller, Owner: owner, Target: owner, Action: ReadDirectory}); err != nil {
		return result, err
	}
	records, err := s.repo.Enrollments(ctx, owner, after, limit)
	if err != nil {
		return result, err
	}
	observed := s.now().UTC()
	if observed.IsZero() {
		return DirectoryPage{}, fabricstore.ErrInvalid
	}
	for _, record := range records {
		result.Cursor = record.Actor.Value.URN
		if record.Actor.Value.Lifecycle != mesh.EnrollmentActive {
			continue
		}
		auth := Authorization{Caller: caller, Owner: owner, Target: record.Actor.Value.URN, Action: ReadDirectory}
		if err := s.allow(ctx, auth); err != nil {
			if errors.Is(err, ErrDenied) {
				continue
			}
			return DirectoryPage{}, err
		}
		var verified definitionresolve.VerifiedDefinition
		if record.Agent != nil {
			verified, err = s.definitions.Load(ctx, record.Agent.Value.Definition)
			if err != nil {
				if ctx.Err() != nil {
					return DirectoryPage{}, ctx.Err()
				}
				if verificationFailure(err) {
					continue
				}
				return DirectoryPage{}, err
			}
			if verified.Pin != record.Agent.Value.Definition || verified.Definition == nil {
				continue
			}
		}
		publication, err := s.advertise(ctx, auth, verified)
		if err != nil {
			return DirectoryPage{}, err
		}
		if !publication.Publish {
			continue
		}
		ids := []string{}
		offered := map[string]bool{}
		if verified.Definition != nil {
			for _, capability := range verified.Definition.Capabilities {
				offered[capability.ID] = true
			}
		}
		seen := map[string]bool{}
		for _, id := range publication.Capabilities {
			if seen[id] {
				return DirectoryPage{}, fabricstore.ErrInvalid
			}
			seen[id] = true
			if offered[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		projection := DirectoryRecord{Authority: record.Actor.Value.Authority.Effective(), URN: record.Actor.Value.URN, Kind: record.Actor.Value.Kind, Lifecycle: record.Actor.Value.Lifecycle, Capabilities: ids, RecordRevision: fmt.Sprintf("%d", record.Actor.Version)}
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
	for i := range result.Records {
		result.Records[i].VerifiedAt = observed
		result.Records[i].ValidUntil = observed.Add(s.directoryTTL)
	}
	return result, nil
}

// Known operational causes take precedence, even when a validation callback
// wrapped them in a content error. Unknown errors are never silently omitted.
func verificationFailure(err error) bool {
	for _, fault := range []error{context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, io.ErrShortWrite, fs.ErrNotExist, fs.ErrExist, fs.ErrPermission, fs.ErrClosed, fs.ErrInvalid, fabricstore.ErrNotFound, fabricstore.ErrConflict, fabricstore.ErrInvalid, definitionresolve.ErrConfiguration} {
		if errors.Is(err, fault) {
			return false
		}
	}
	var pathError *fs.PathError
	var networkError net.Error
	var systemError syscall.Errno
	if errors.As(err, &pathError) || errors.As(err, &networkError) || errors.As(err, &systemError) {
		return false
	}
	return errors.Is(err, definitionresolve.ErrPinMismatch) || errors.Is(err, definitionresolve.ErrContent)
}
