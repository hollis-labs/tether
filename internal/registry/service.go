package registry

// service.go — registry service core (T-v060-01-03). The Service owns the
// validation, URN minting, and UpdateSelf partial-merge semantics (D5)
// that the storage layer deliberately doesn't carry. Storage is a thin
// data-access surface; merge rules and field-name dispatch live here.
//
// Storage interface. The Service depends on the unexported storageBackend
// interface (not *Storage directly) so tests can stub specific behavior —
// in particular, forcing a URN collision sequence in Register — without
// spinning a real SQLite. Production wiring is NewService(*Storage); the
// struct satisfies the interface through method promotion.
//
// Concurrency. Per internal/store/sqlite.go, the *sql.DB pool is configured
// with MaxOpenConns=1, which serializes every database touch through one
// connection. Concurrent Service.UpdateSelf calls on the same URN therefore
// serialize at the connection-pool level: each storage op is internally
// atomic, and last-writer-wins on each field when two patches collide.
// No per-URN mutex is required (or warranted) for v1. The cross-table case
// (a single UpdateSelf that touches both scalars and array patches) is not
// atomic-as-a-unit — each storage op runs in its own transaction and
// partial progress on multi-op failure is acceptable for v060-01 per the
// sprint spec.
//
// D5 dispatch table. UpdateSelf maps each present ArrayPatch to storage ops:
//
//	Mode             Capabilities          Skills              Links
//	──────────────── ─────────────────────  ────────────────── ────────────────
//	ArrayModeReplace ReplaceCapabilities    ReplaceSkills      ReplaceLinks
//	ArrayModeAppend  AppendCapabilities     AppendSkills       AppendLinks
//	ArrayModeRemove  RemoveCapabilities     RemoveSkills(name) RemoveLinks
//
// Empty Value on any mode is a no-op (D5: "Empty value is a no-op") and the
// storage call is skipped entirely.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidRequest is returned for caller-supplied input that violates a
// Service-level invariant (caller-supplied URN, missing display_name,
// malformed skill, etc.). Callers use errors.Is.
var ErrInvalidRequest = errors.New("registry: invalid request")

// storageBackend is the unexported storage surface the Service depends on.
// Mirrors the public methods of *Storage that the Service consumes; lets
// tests stub URNExists / InsertProfile in isolation for collision-retry
// coverage. *Storage satisfies this interface through method promotion.
type storageBackend interface {
	InsertProfile(ctx context.Context, p Profile) error
	GetProfile(ctx context.Context, urn string) (Profile, error)
	URNExists(ctx context.Context, urn string) (bool, error)
	UpdateProfileFields(ctx context.Context, urn string, fields map[string]any) error
	ReplaceCapabilities(ctx context.Context, urn string, caps []string) error
	ReplaceSkills(ctx context.Context, urn string, skills []Skill) error
	ReplaceLinks(ctx context.Context, urn string, links []Link) error
	AppendCapabilities(ctx context.Context, urn string, caps []string) error
	AppendSkills(ctx context.Context, urn string, skills []Skill) error
	AppendLinks(ctx context.Context, urn string, links []Link) error
	RemoveCapabilities(ctx context.Context, urn string, caps []string) error
	RemoveSkills(ctx context.Context, urn string, names []string) error
	RemoveLinks(ctx context.Context, urn string, links []Link) error
	SoftDelete(ctx context.Context, urn string) error
}

// Service is the registry service core: validation, URN minting, and the
// UpdateSelf partial-merge implementation. Hold one per process.
type Service struct {
	storage storageBackend
}

// NewService binds a Service to a production *Storage. Tests bypass this
// constructor via export_test.go to inject a stub storageBackend (used
// only for URN-collision-retry coverage).
func NewService(s *Storage) *Service {
	return &Service{storage: s}
}

// Register validates the inbound Profile, mints a URN of the correct
// prefix, and inserts the row. The returned Profile is the canonical
// reloaded row (with all storage-layer defaults applied — status,
// mux_instance_id, created_at/updated_at, child arrays populated to their
// inserted state).
func (s *Service) Register(ctx context.Context, kind Kind, p Profile) (Profile, error) {
	if p.URN != "" {
		return Profile{}, fmt.Errorf("registry: register: %w: caller-supplied URN not allowed; server mints", ErrInvalidRequest)
	}
	switch kind {
	case KindAgent, KindProject:
	default:
		return Profile{}, fmt.Errorf("registry: register: %w: unsupported kind %q", ErrInvalidRequest, string(kind))
	}
	if p.DisplayName == "" {
		return Profile{}, fmt.Errorf("registry: register: %w: display_name is required", ErrInvalidRequest)
	}
	for i, sk := range p.Skills {
		if sk.Name == "" {
			return Profile{}, fmt.Errorf("registry: register: %w: skills[%d].name is required (D13)", ErrInvalidRequest, i)
		}
		if sk.LearnedAt.IsZero() {
			return Profile{}, fmt.Errorf("registry: register: %w: skills[%d].learned_at is required (D13)", ErrInvalidRequest, i)
		}
	}

	var (
		minted string
		err    error
	)
	switch kind {
	case KindAgent:
		minted, err = MintAgentURN(ctx, s.storage.URNExists)
	case KindProject:
		minted, err = MintProjectURN(ctx, s.storage.URNExists)
	}
	if err != nil {
		// ErrMintExhausted (and context errors) propagate verbatim so callers
		// can errors.Is them directly.
		return Profile{}, err
	}

	p.URN = minted
	p.Kind = kind
	if p.LastUpdatedBy == "" {
		// Placeholder per sprint spec: token-based identity is v060-02.
		p.LastUpdatedBy = "system:register"
	}

	if err := s.storage.InsertProfile(ctx, p); err != nil {
		return Profile{}, fmt.Errorf("registry: register: insert: %w", err)
	}
	out, err := s.storage.GetProfile(ctx, p.URN)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: register: reload: %w", err)
	}
	return out, nil
}

// Lookup returns the Profile for urn. ErrNotFound propagates verbatim so
// callers can errors.Is it.
func (s *Service) Lookup(ctx context.Context, urn string) (Profile, error) {
	return s.storage.GetProfile(ctx, urn)
}

// UpdateSelf applies a D5 partial-merge patch to the row at urn and
// returns the reloaded Profile. Scalar pointer fields → column updates;
// array patches → dispatch on Mode (see package comment). Empty array
// Value is a no-op. LastUpdatedBy is required.
func (s *Service) UpdateSelf(ctx context.Context, urn string, patch UpdatePatch) (Profile, error) {
	if patch.LastUpdatedBy == "" {
		return Profile{}, fmt.Errorf("registry: update_self: %w: last_updated_by required on UpdateSelf", ErrInvalidRequest)
	}
	// Existence check up front so ErrNotFound surfaces cleanly instead of
	// being inferred from a downstream "rows affected = 0".
	if _, err := s.storage.GetProfile(ctx, urn); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Profile{}, err
		}
		return Profile{}, fmt.Errorf("registry: update_self: pre-check: %w", err)
	}

	fields := map[string]any{}
	if patch.DisplayName != nil {
		fields["display_name"] = *patch.DisplayName
	}
	if patch.Title != nil {
		fields["title"] = *patch.Title
	}
	if patch.Role != nil {
		fields["role"] = *patch.Role
	}
	if patch.Description != nil {
		fields["description"] = *patch.Description
	}
	if patch.Avatar != nil {
		fields["avatar"] = *patch.Avatar
	}
	if patch.Project != nil {
		fields["project"] = *patch.Project
	}
	if patch.Status != nil {
		fields["status"] = string(*patch.Status)
	}
	if patch.HealthStatus != nil {
		fields["health_status"] = *patch.HealthStatus
	}
	if patch.LastSeenAt != nil {
		// Match storage.go's nullIfTimePtr formatting so a zero time clears
		// the column. Non-zero times go through the same RFC3339Nano UTC
		// path the rest of the package uses.
		if patch.LastSeenAt.IsZero() {
			fields["last_seen_at"] = nil
		} else {
			fields["last_seen_at"] = patch.LastSeenAt.UTC().Format(time.RFC3339Nano)
		}
	}
	if patch.HostAddress != nil {
		fields["host_address"] = *patch.HostAddress
	}
	if len(patch.KindMeta) > 0 {
		fields["kind_meta_json"] = string(patch.KindMeta)
	}
	// last_updated_by is always written so the row reflects the originator
	// of this update — even when the patch carries no other scalar changes.
	fields["last_updated_by"] = patch.LastUpdatedBy

	if err := s.storage.UpdateProfileFields(ctx, urn, fields); err != nil {
		return Profile{}, fmt.Errorf("registry: update_self: scalar: %w", err)
	}

	if patch.Capabilities != nil && len(patch.Capabilities.Value) > 0 {
		if err := s.applyCapabilitiesPatch(ctx, urn, *patch.Capabilities); err != nil {
			return Profile{}, err
		}
	}
	if patch.Skills != nil && len(patch.Skills.Value) > 0 {
		if err := s.applySkillsPatch(ctx, urn, *patch.Skills); err != nil {
			return Profile{}, err
		}
	}
	if patch.Links != nil && len(patch.Links.Value) > 0 {
		if err := s.applyLinksPatch(ctx, urn, *patch.Links); err != nil {
			return Profile{}, err
		}
	}

	out, err := s.storage.GetProfile(ctx, urn)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: update_self: reload: %w", err)
	}
	return out, nil
}

// Deregister soft-deletes the row at urn (D11). Child tables are
// intentionally left intact; the returned Profile carries the now-
// deprecated status.
func (s *Service) Deregister(ctx context.Context, urn string) (Profile, error) {
	if _, err := s.storage.GetProfile(ctx, urn); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Profile{}, err
		}
		return Profile{}, fmt.Errorf("registry: deregister: pre-check: %w", err)
	}
	if err := s.storage.SoftDelete(ctx, urn); err != nil {
		return Profile{}, fmt.Errorf("registry: deregister: soft delete: %w", err)
	}
	out, err := s.storage.GetProfile(ctx, urn)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: deregister: reload: %w", err)
	}
	return out, nil
}

// ─── array patch dispatch ────────────────────────────────────────────────────

func (s *Service) applyCapabilitiesPatch(ctx context.Context, urn string, p ArrayPatch[string]) error {
	switch p.Mode {
	case ArrayModeReplace:
		return wrapPatch("capabilities", "replace", s.storage.ReplaceCapabilities(ctx, urn, p.Value))
	case ArrayModeAppend:
		return wrapPatch("capabilities", "append", s.storage.AppendCapabilities(ctx, urn, p.Value))
	case ArrayModeRemove:
		return wrapPatch("capabilities", "remove", s.storage.RemoveCapabilities(ctx, urn, p.Value))
	default:
		return fmt.Errorf("registry: update_self: %w: capabilities mode %q", ErrInvalidRequest, p.Mode)
	}
}

func (s *Service) applySkillsPatch(ctx context.Context, urn string, p ArrayPatch[Skill]) error {
	switch p.Mode {
	case ArrayModeReplace:
		return wrapPatch("skills", "replace", s.storage.ReplaceSkills(ctx, urn, p.Value))
	case ArrayModeAppend:
		return wrapPatch("skills", "append", s.storage.AppendSkills(ctx, urn, p.Value))
	case ArrayModeRemove:
		// RemoveSkills matches on name only (D5), so extract names from the
		// inbound Skill structs and discard the rest.
		names := make([]string, 0, len(p.Value))
		for _, sk := range p.Value {
			names = append(names, sk.Name)
		}
		return wrapPatch("skills", "remove", s.storage.RemoveSkills(ctx, urn, names))
	default:
		return fmt.Errorf("registry: update_self: %w: skills mode %q", ErrInvalidRequest, p.Mode)
	}
}

func (s *Service) applyLinksPatch(ctx context.Context, urn string, p ArrayPatch[Link]) error {
	switch p.Mode {
	case ArrayModeReplace:
		return wrapPatch("links", "replace", s.storage.ReplaceLinks(ctx, urn, p.Value))
	case ArrayModeAppend:
		return wrapPatch("links", "append", s.storage.AppendLinks(ctx, urn, p.Value))
	case ArrayModeRemove:
		return wrapPatch("links", "remove", s.storage.RemoveLinks(ctx, urn, p.Value))
	default:
		return fmt.Errorf("registry: update_self: %w: links mode %q", ErrInvalidRequest, p.Mode)
	}
}

func wrapPatch(field, mode string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("registry: update_self: %s %s: %w", field, mode, err)
}
