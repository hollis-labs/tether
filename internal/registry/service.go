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
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
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
	FindByCallbackTarget(ctx context.Context, target string) (Profile, error)
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
	BumpCachedAt(ctx context.Context, urn string, at time.Time) error
	Search(ctx context.Context, kind Kind, f Filter) ([]Profile, error)
}

// Service is the registry service core: validation, URN minting, and the
// UpdateSelf partial-merge implementation. Hold one per process.
//
// Resolvers (T-v060-01-04). The resolvers map dispatches Sync to a Resolver
// keyed on the row's callback Scheme. Callers wire resolvers at
// construction time via WithResolver — see NewService. The map is read-
// only after construction; v1 has no hot-swap or dynamic registration.
type Service struct {
	storage   storageBackend
	resolvers map[string]Resolver
}

// ServiceOption configures a Service at construction time. v1 ships
// WithResolver; future options (rate limit, audit hook) extend the same
// shape without breaking the variadic signature.
type ServiceOption func(*Service)

// WithResolver registers a Resolver under its Scheme() key. Multiple
// WithResolver options compose; a later WithResolver for the same scheme
// replaces the earlier one (last-wins).
func WithResolver(r Resolver) ServiceOption {
	return func(s *Service) {
		if s.resolvers == nil {
			s.resolvers = map[string]Resolver{}
		}
		s.resolvers[r.Scheme()] = r
	}
}

// NewService binds a Service to a production *Storage. Tests bypass this
// constructor via export_test.go to inject a stub storageBackend (used
// only for URN-collision-retry coverage).
//
// Optional ServiceOptions configure Sync resolvers — without at least one
// Resolver, Sync returns ErrNoResolver. Production wiring should pass
// WithResolver(NewFileResolver(...)) and WithResolver(NewCLIResolver()).
func NewService(s *Storage, opts ...ServiceOption) *Service {
	svc := &Service{
		storage:   s,
		resolvers: map[string]Resolver{},
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
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
	case KindGroup:
		// Group Register handler lands in v060-05 T-02 (sprint v060-05).
		// Until then, surface a clear invalid-request rather than silently
		// minting nothing.
		return Profile{}, fmt.Errorf("registry: register: %w: kind=group is not yet supported via Register (v060-05 T-02)", ErrInvalidRequest)
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

// Search returns profiles of the given kind matching filter, ordered
// alphabetically by display_name. Thin wrapper over storage.Search; the
// API layer depends on this rather than *Storage directly so the handler
// surface stays decoupled from the data layer.
//
// Empty result returns a non-nil zero-length slice so callers/HTTP
// marshaling produce a stable `[]` instead of `null`.
func (s *Service) Search(ctx context.Context, kind Kind, f Filter) ([]Profile, error) {
	switch kind {
	case KindAgent, KindProject:
	default:
		return nil, fmt.Errorf("registry: search: %w: unsupported kind %q", ErrInvalidRequest, string(kind))
	}
	out, err := s.storage.Search(ctx, kind, f)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return []Profile{}, nil
	}
	return out, nil
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

// ─── Sync ────────────────────────────────────────────────────────────────────

// Sync refreshes the row's thin-profile columns by calling the registered
// Resolver for the row's Callback.Scheme. The Resolver returns raw payload
// bytes; Sync parses them (JSON or YAML — sniffed on first non-whitespace
// byte) into a fresh Profile and applies a full-REPLACE UpdatePatch via
// UpdateSelf. cached_at is bumped via BumpCachedAt after the update.
//
// Raw payload is NOT cached (D18): substrate ops-store files commonly
// contain plaintext secrets, so the registry never echoes the bytes back
// to state.db. Only the thin-profile columns (display_name, role, etc.)
// + capabilities/skills/links arrays are mirrored.
//
// Empty-array nuance. UpdateSelf treats an ArrayPatch with len(Value)==0
// as a no-op (D5), so Sync cannot clear arrays in v1. A payload that
// omits "capabilities" leaves existing capabilities intact; a payload
// with an explicit "capabilities": [] also leaves them intact. v060-02
// or v060-03 may add an explicit "clear" semantic if substrates need it.
//
// Errors:
//   - ErrNotFound — urn not in the registry.
//   - ErrNoCallback — the row exists but has no callback (HTTP 204).
//   - ErrNoResolver — no Resolver registered for the row's Scheme.
//   - ErrPayloadInvalid / ErrPayloadTooLarge / ErrPathOutsideRoot —
//     resolver-layer errors propagate verbatim.
//   - other errors wrapped from storage / Resolve.
func (s *Service) Sync(ctx context.Context, urn string) (Profile, error) {
	existing, err := s.storage.GetProfile(ctx, urn)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Profile{}, err
		}
		return Profile{}, fmt.Errorf("registry: sync: lookup: %w", err)
	}
	if existing.Callback == nil {
		return Profile{}, fmt.Errorf("registry: sync %q: %w", urn, ErrNoCallback)
	}

	resolver, ok := s.resolvers[existing.Callback.Scheme]
	if !ok {
		return Profile{}, fmt.Errorf("registry: sync %q: %w: %q", urn, ErrNoResolver, existing.Callback.Scheme)
	}

	payload, err := resolver.Resolve(ctx, existing.Callback.Target)
	if err != nil {
		// Resolver errors already carry the registry: file/cli resolver:
		// prefix and the sentinel wrap, so propagate verbatim.
		return Profile{}, err
	}

	parsed, err := parseSyncPayload(payload)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: sync %q: %w", urn, err)
	}

	patch := buildSyncPatch(parsed)
	if _, err := s.UpdateSelf(ctx, urn, patch); err != nil {
		return Profile{}, fmt.Errorf("registry: sync %q: update_self: %w", urn, err)
	}

	if err := s.storage.BumpCachedAt(ctx, urn, time.Now().UTC()); err != nil {
		return Profile{}, fmt.Errorf("registry: sync %q: bump cached_at: %w", urn, err)
	}

	out, err := s.storage.GetProfile(ctx, urn)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: sync %q: reload: %w", urn, err)
	}
	return out, nil
}

// parseSyncPayload sniffs the first non-whitespace byte of the payload to
// pick a decoder: '{' or '[' → JSON direct into Profile; otherwise → YAML
// via an intermediate map (so Profile's existing JSON tags drive the
// field mapping without needing a parallel set of yaml tags).
//
// A parse failure wraps ErrPayloadInvalid so callers can errors.Is.
func parseSyncPayload(payload []byte) (Profile, error) {
	if len(payload) == 0 {
		return Profile{}, fmt.Errorf("%w: empty payload", ErrPayloadInvalid)
	}
	// Find the first non-whitespace byte to decide JSON vs YAML.
	var first byte
	for _, b := range payload {
		if !unicode.IsSpace(rune(b)) {
			first = b
			break
		}
	}
	var p Profile
	if first == '{' || first == '[' {
		if err := json.Unmarshal(payload, &p); err != nil {
			return Profile{}, fmt.Errorf("%w: json: %w", ErrPayloadInvalid, err)
		}
		return p, nil
	}
	// YAML route. Profile has JSON tags but no YAML tags; yaml.v3 would
	// otherwise look for lowercased field names. Decode into a generic
	// map first, then re-marshal to JSON so Profile's JSON tags pick up
	// the snake_case field names from the YAML document.
	var generic map[string]any
	if err := yaml.Unmarshal(payload, &generic); err != nil {
		return Profile{}, fmt.Errorf("%w: yaml: %w", ErrPayloadInvalid, err)
	}
	canon, err := json.Marshal(generic)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: yaml->json: %w", ErrPayloadInvalid, err)
	}
	if err := json.Unmarshal(canon, &p); err != nil {
		return Profile{}, fmt.Errorf("%w: yaml->profile: %w", ErrPayloadInvalid, err)
	}
	return p, nil
}

// buildSyncPatch translates a parsed Profile into a full-REPLACE
// UpdatePatch. Scalar fields with empty values are omitted from the patch
// (avoids unintentionally clearing columns on a sparse payload); array
// fields are always wrapped in ArrayModeReplace, though len(Value)==0
// hits the UpdateSelf no-op path (see Sync godoc for the nuance).
//
// LastUpdatedBy is hard-coded to "system:sync" — v060-02's token-based
// identity supersedes this placeholder.
func buildSyncPatch(p Profile) UpdatePatch {
	patch := UpdatePatch{LastUpdatedBy: "system:sync"}
	if p.DisplayName != "" {
		v := p.DisplayName
		patch.DisplayName = &v
	}
	if p.Title != "" {
		v := p.Title
		patch.Title = &v
	}
	if p.Role != "" {
		v := p.Role
		patch.Role = &v
	}
	if p.Description != "" {
		v := p.Description
		patch.Description = &v
	}
	if p.Avatar != "" {
		v := p.Avatar
		patch.Avatar = &v
	}
	if p.Project != "" {
		v := p.Project
		patch.Project = &v
	}
	if p.Status != "" {
		v := p.Status
		patch.Status = &v
	}
	if p.HealthStatus != "" {
		v := p.HealthStatus
		patch.HealthStatus = &v
	}
	if p.HostAddress != "" {
		v := p.HostAddress
		patch.HostAddress = &v
	}
	if p.LastSeenAt != nil {
		t := *p.LastSeenAt
		patch.LastSeenAt = &t
	}
	if len(p.KindMeta) > 0 {
		patch.KindMeta = p.KindMeta
	}
	patch.Capabilities = &ArrayPatch[string]{Mode: ArrayModeReplace, Value: p.Capabilities}
	patch.Skills = &ArrayPatch[Skill]{Mode: ArrayModeReplace, Value: p.Skills}
	patch.Links = &ArrayPatch[Link]{Mode: ArrayModeReplace, Value: p.Links}
	return patch
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
