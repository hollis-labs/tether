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
	"slices"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ErrInvalidRequest is returned for caller-supplied input that violates a
// Service-level invariant (caller-supplied URN, missing display_name,
// malformed skill, etc.). Callers use errors.Is.
var ErrInvalidRequest = errors.New("registry: invalid request")

// ErrForbidden is returned when a caller lacks the role required for the
// requested operation (v060-05: archive a group, invite/kick members,
// post to a group as a non-member). Callers use errors.Is and the HTTP
// layer maps to 403.
var ErrForbidden = errors.New("registry: forbidden")

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
	LookupExternalIDsForURN(ctx context.Context, urn string) ([]ExternalID, error)
	LookupURNByExternalID(ctx context.Context, kind Kind, externalID, substrate string) (string, bool, error)
	AttachExternalID(ctx context.Context, urn, substrate, externalID string) error
	DetachExternalID(ctx context.Context, urn, substrate string) error
	RegisterWithExternalKey(ctx context.Context, p Profile, substrate, externalID string) (urn string, created bool, err error)

	// T02 leased runtime bindings.
	LeaseBinding(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, visibility PublicationVisibility, ttl time.Duration) (RuntimeBinding, error)
	// T07: atomically guarded variant closing a TOCTOU window a distinct
	// review pass found in the external HTTP binding-lease surface.
	LeaseBindingUnlessVisibility(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, visibility PublicationVisibility, ttl time.Duration, blocked ...PublicationVisibility) (RuntimeBinding, error)
	RenewLease(ctx context.Context, bindingID string, ttl time.Duration) (RuntimeBinding, error)
	RevokeBinding(ctx context.Context, bindingID string) error
	CurrentBinding(ctx context.Context, targetURN string) (RuntimeBinding, error)
	ListBindingsForTarget(ctx context.Context, targetURN string) ([]RuntimeBinding, error)

	// T04 scoped role/slot bindings.
	SetScopedBinding(ctx context.Context, scope, slot string, targetURNs []string, relationship json.RawMessage, createdBy string) (ScopedBinding, error)
	ResolveScopedBinding(ctx context.Context, scope, slot string) (ScopedBinding, error)
	ResolveScopedBindingSingle(ctx context.Context, scope, slot string) (string, ScopedBinding, error)
	ListScopedBindingRevisions(ctx context.Context, scope, slot string) ([]ScopedBinding, error)

	// T04 group-fanout delivery mapping.
	SetGroupMessageDeliveryMapping(ctx context.Context, messageID, deliveryMessageID string) error

	// v060-05 group ops (T-02 + T-03 + T-04).
	InsertGroupWithOwner(ctx context.Context, p Profile, ownerURN string) error
	InsertGroupMember(ctx context.Context, grpURN, memberURN string, role MemberRole, joinedAt time.Time) error
	GroupMemberRole(ctx context.Context, grpURN, memberURN string) (MemberRole, bool, error)
	ListGroupsForMember(ctx context.Context, memberURN string) ([]Profile, error)
	SetProfileStatus(ctx context.Context, urn string, status Status) error
	ListMembers(ctx context.Context, grpURN string) ([]GroupMember, error)
	RemoveGroupMember(ctx context.Context, grpURN, memberURN string) error
	UpdateGroupMemberRole(ctx context.Context, grpURN, memberURN string, role MemberRole) error
	CountModeratorsExcluding(ctx context.Context, grpURN, excludeURN string) (int, error)
	InsertGroupMessage(ctx context.Context, grpURN, fromURN, kind, threadID, contentType string, payload json.RawMessage) (GroupMessage, error)
	ListGroupMessages(ctx context.Context, grpURN string, sinceSeq int64, threadID string, limit int, joinedAt time.Time) ([]GroupMessage, error)
	BumpGroupReadCursor(ctx context.Context, grpURN, memberURN string, upToSeq int64) error
	GroupMemberJoinedAt(ctx context.Context, grpURN, memberURN string) (time.Time, bool, error)
	GroupMemberLastReadSeq(ctx context.Context, grpURN, memberURN string) (int64, bool, error)
	ListMentionsForMember(ctx context.Context, memberURN string, sinceTS time.Time, limit int) ([]GroupMessage, error)
	FindByDisplayName(ctx context.Context, name string) ([]Profile, error)
}

// Service is the registry service core: validation, URN minting, and the
// UpdateSelf partial-merge implementation. Hold one per process.
//
// Resolvers (T-v060-01-04). The resolvers map dispatches Sync to a Resolver
// keyed on the row's callback Scheme. Callers wire resolvers at
// construction time via WithResolver — see NewService. The map is read-
// only after construction; v1 has no hot-swap or dynamic registration.
//
// Mention parser (T-v060-05-05). If installed via WithMentionParser, the
// Service invokes ParseAndDispatch after every successful SendToGroup
// commit. T-04 reserves the seam; T-05 implements the parser.
type Service struct {
	storage       storageBackend
	resolvers     map[string]Resolver
	mentionParser MentionParser
	// deliveryStore is the T04 group-fanout hook (see group_fanout.go). A
	// pure go-messaging library interface -- injecting it does not create a
	// dependency on internal/store, keeping internal/registry's existing
	// zero-coupling to Tether's own session/store packages intact.
	deliveryStore GroupFanoutDeliveryStore
}

// Mention is a resolved @-token extracted from a group message payload.
// ResolvedURN is empty when Source=="urn" and the URN doesn't exist in
// the registry (unknown URN — the parser keeps it as a candidate but
// dispatch logs and skips).
type Mention struct {
	Token       string // the raw @-token as it appeared (without the @)
	ResolvedURN string // the URN to deliver a notice to
	Source      string // "urn" (full-form match) or "display_name" (short-form)
}

// MentionParser is the two-phase hook SendToGroup calls around the
// message-row commit.
//
//   - Parse runs BEFORE commit. It scans payload for @-tokens, resolves
//     short-forms via registry Lookup, and returns the unique resolved
//     mentions. Returning ErrAmbiguousMention causes SendToGroup to
//     abort with the error (HTTP 400 per sprint) — the row is NOT
//     written. Returning other errors also aborts; nil + empty slice
//     is the no-mentions case.
//
//   - Dispatch runs AFTER commit, with the just-inserted GroupMessage
//     and the mentions Parse returned. Errors from Dispatch are
//     fire-and-forget per D11/D12 — the group message itself was
//     successfully delivered.
type MentionParser interface {
	Parse(ctx context.Context, payload json.RawMessage, groupURN string) ([]Mention, error)
	Dispatch(ctx context.Context, gm GroupMessage, mentions []Mention)
}

// ErrAmbiguousMention indicates a short-form @-token resolves to more
// than one URN. SendToGroup returns this verbatim so the HTTP layer can
// expose the candidate list in the response body (sprint: "400
// ambiguous_mention" + helpful payload listing candidate URNs).
type ErrAmbiguousMention struct {
	Token      string   // the @-token (without the @)
	Candidates []string // the URNs that share this display_name
}

// Error implements error.
func (e *ErrAmbiguousMention) Error() string {
	return fmt.Sprintf("registry: ambiguous mention %q resolves to %d URNs: %v", e.Token, len(e.Candidates), e.Candidates)
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

// WithMentionParser installs the post-SendToGroup mention dispatch hook
// (v060-05 T-05). If not set, mention parsing is skipped — the Service
// still accepts group sends, but no notice envelopes are emitted to
// mentioned members' personal inboxes.
func WithMentionParser(p MentionParser) ServiceOption {
	return func(s *Service) {
		s.mentionParser = p
	}
}

// SetMentionParser installs (or replaces) the post-SendToGroup mention
// dispatch hook after construction. v060-05 T-06 wiring uses this so the
// composition root (app.Service) can construct the parser AFTER the
// registry.Service is built — the parser depends on registry.Service for
// resolution, so the construction order is unavoidable.
//
// Passing nil clears the hook (mention parsing reverts to skipped).
// Concurrent SendToGroup callers may observe either the old or new parser
// during the swap; v1 has no atomic fence because composition-root
// wiring runs once at daemon startup.
func (s *Service) SetMentionParser(p MentionParser) {
	s.mentionParser = p
}

// WithDeliveryStore installs the T04 group-fanout hook at construction
// time. See group_fanout.go.
func WithDeliveryStore(d GroupFanoutDeliveryStore) ServiceOption {
	return func(s *Service) {
		s.deliveryStore = d
	}
}

// SetDeliveryStore installs (or replaces) the group-fanout hook after
// construction -- mirrors SetMentionParser's rationale: the composition
// root builds internal/store.Store (which owns the delivery core) and
// internal/registry.Service somewhat independently, so wiring one into the
// other after both exist avoids an artificial construction-order
// dependency. Passing nil disables fanout (SendToGroup still succeeds;
// only the durable per-recipient obligation is skipped).
func (s *Service) SetDeliveryStore(d GroupFanoutDeliveryStore) {
	s.deliveryStore = d
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
//
// For kind=group (v060-05 T-02): Profile.LastUpdatedBy carries the
// creator URN (must be a well-formed registry URN that already exists
// in registry_entries). On success, the creator is auto-added to
// group_members with role='owner', inside the same transaction as the
// profile insert. Token-based auth (v060-03) will replace the explicit
// caller-supplied creator URN.
func (s *Service) Register(ctx context.Context, kind Kind, p Profile) (Profile, error) {
	if p.URN != "" {
		return Profile{}, fmt.Errorf("registry: register: %w: caller-supplied URN not allowed; server mints", ErrInvalidRequest)
	}
	switch kind {
	case KindAgent, KindProject, KindGroup:
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

	if kind == KindGroup {
		return s.registerGroup(ctx, p)
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
		// Unreachable — the kind==KindGroup early-return above takes the
		// group path. Listed here so the exhaustive linter is satisfied.
		return Profile{}, fmt.Errorf("registry: register: %w: unreachable group path", ErrInvalidRequest)
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

// registerGroup is the kind=group path of Register. Validates the creator
// URN, mints a group URN, and inserts profile+owner-member atomically.
func (s *Service) registerGroup(ctx context.Context, p Profile) (Profile, error) {
	creator := p.LastUpdatedBy
	if creator == "" {
		return Profile{}, fmt.Errorf("registry: register group: %w: last_updated_by is required and must carry the creator URN (v060-05; v060-03 token auth will replace)", ErrInvalidRequest)
	}
	if _, err := ParseRegistryURN(creator); err != nil {
		return Profile{}, fmt.Errorf("registry: register group: %w: invalid creator URN %q: %w", ErrInvalidRequest, creator, err)
	}
	// Creator must exist in registry. Reject `kind=group` creators in v1 —
	// only agents/projects can create groups (a group creating a group is
	// out-of-band and not supported v1).
	creatorProfile, err := s.storage.GetProfile(ctx, creator)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Profile{}, fmt.Errorf("registry: register group: %w: creator URN %q not in registry", ErrInvalidRequest, creator)
		}
		return Profile{}, fmt.Errorf("registry: register group: creator lookup: %w", err)
	}
	if creatorProfile.Kind == KindGroup {
		return Profile{}, fmt.Errorf("registry: register group: %w: a group cannot create a group", ErrInvalidRequest)
	}

	minted, err := MintGroupURN(ctx, "", s.storage.URNExists)
	if err != nil {
		return Profile{}, err
	}

	p.URN = minted
	p.Kind = KindGroup

	if err := s.storage.InsertGroupWithOwner(ctx, p, creator); err != nil {
		return Profile{}, fmt.Errorf("registry: register group: %w", err)
	}
	out, err := s.storage.GetProfile(ctx, p.URN)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: register group: reload: %w", err)
	}
	return out, nil
}

// ListGroupsForMember returns the group profiles memberURN belongs to,
// ordered alphabetically by display_name. Status filter NOT applied —
// archived groups are still visible to their former members (mailbox
// continues to function as read-only per D9).
func (s *Service) ListGroupsForMember(ctx context.Context, memberURN string) ([]Profile, error) {
	if memberURN == "" {
		return nil, fmt.Errorf("registry: list groups for member: %w: memberURN required", ErrInvalidRequest)
	}
	if _, err := ParseRegistryURN(memberURN); err != nil {
		return nil, fmt.Errorf("registry: list groups for member: %w: invalid URN: %w", ErrInvalidRequest, err)
	}
	out, err := s.storage.ListGroupsForMember(ctx, memberURN)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Profile{}
	}
	return out, nil
}

// ArchiveGroup sets the group's status to "archived" (D9 — group becomes
// read-only). Caller must be the group's owner or a moderator (D8 — only
// owner/moderator can invite/kick/archive). Returns the reloaded profile.
//
// Errors:
//   - ErrInvalidRequest — URN malformed or not a group URN.
//   - ErrNotFound       — grpURN not in registry.
//   - ErrForbidden      — byURN is not owner or moderator of the group.
func (s *Service) ArchiveGroup(ctx context.Context, grpURN, byURN string) (Profile, error) {
	if grpURN == "" || byURN == "" {
		return Profile{}, fmt.Errorf("registry: archive group: %w: grpURN + byURN required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return Profile{}, fmt.Errorf("registry: archive group: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	existing, err := s.storage.GetProfile(ctx, grpURN)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Profile{}, err
		}
		return Profile{}, fmt.Errorf("registry: archive group: lookup: %w", err)
	}
	if existing.Kind != KindGroup {
		return Profile{}, fmt.Errorf("registry: archive group: %w: row is not a group (kind=%q)", ErrInvalidRequest, existing.Kind)
	}
	role, ok, err := s.storage.GroupMemberRole(ctx, grpURN, byURN)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: archive group: role check: %w", err)
	}
	if !ok || (role != MemberRoleOwner && role != MemberRoleModerator) {
		return Profile{}, fmt.Errorf("registry: archive group: %w: byURN must be owner or moderator (got role=%q, member=%v)", ErrForbidden, role, ok)
	}
	if err := s.storage.SetProfileStatus(ctx, grpURN, StatusArchived); err != nil {
		return Profile{}, fmt.Errorf("registry: archive group: %w", err)
	}
	out, err := s.storage.GetProfile(ctx, grpURN)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: archive group: reload: %w", err)
	}
	return out, nil
}

// ─── group membership (v060-05 T-03) ─────────────────────────────────────────

// AddMember inserts memberURN as a member of grpURN. byURN must be the
// group's owner or a moderator (D8). memberURN must be a valid
// registry URN that exists. role defaults to MemberRoleMember.
//
// Errors:
//   - ErrInvalidRequest — bad URN, group URN not a group, member already in group.
//   - ErrNotFound       — grpURN not in registry; memberURN not in registry.
//   - ErrForbidden      — byURN is not owner or moderator of the group.
func (s *Service) AddMember(ctx context.Context, grpURN, memberURN, byURN string, role MemberRole) (GroupMember, error) {
	if grpURN == "" || memberURN == "" || byURN == "" {
		return GroupMember{}, fmt.Errorf("registry: add member: %w: grpURN + memberURN + byURN required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return GroupMember{}, fmt.Errorf("registry: add member: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	if _, err := ParseRegistryURN(memberURN); err != nil {
		return GroupMember{}, fmt.Errorf("registry: add member: %w: invalid memberURN: %w", ErrInvalidRequest, err)
	}
	if role == "" {
		role = MemberRoleMember
	}
	switch role {
	case MemberRoleMember, MemberRoleModerator, MemberRoleOwner:
	default:
		return GroupMember{}, fmt.Errorf("registry: add member: %w: invalid role %q", ErrInvalidRequest, role)
	}
	// Group must exist + member must exist.
	if _, err := s.storage.GetProfile(ctx, grpURN); err != nil {
		if errors.Is(err, ErrNotFound) {
			return GroupMember{}, err
		}
		return GroupMember{}, fmt.Errorf("registry: add member: group lookup: %w", err)
	}
	if _, err := s.storage.GetProfile(ctx, memberURN); err != nil {
		if errors.Is(err, ErrNotFound) {
			return GroupMember{}, fmt.Errorf("registry: add member: %w: memberURN %q not in registry", ErrNotFound, memberURN)
		}
		return GroupMember{}, fmt.Errorf("registry: add member: member lookup: %w", err)
	}
	// byURN must be owner/moderator of grpURN.
	byRole, ok, err := s.storage.GroupMemberRole(ctx, grpURN, byURN)
	if err != nil {
		return GroupMember{}, fmt.Errorf("registry: add member: by-role check: %w", err)
	}
	if !ok || (byRole != MemberRoleOwner && byRole != MemberRoleModerator) {
		return GroupMember{}, fmt.Errorf("registry: add member: %w: byURN must be owner or moderator (role=%q, member=%v)", ErrForbidden, byRole, ok)
	}
	// Reject duplicate membership.
	if _, present, err := s.storage.GroupMemberRole(ctx, grpURN, memberURN); err != nil {
		return GroupMember{}, fmt.Errorf("registry: add member: existing-membership check: %w", err)
	} else if present {
		return GroupMember{}, fmt.Errorf("registry: add member: %w: memberURN %q already in group", ErrInvalidRequest, memberURN)
	}
	now := time.Now().UTC()
	if err := s.storage.InsertGroupMember(ctx, grpURN, memberURN, role, now); err != nil {
		return GroupMember{}, fmt.Errorf("registry: add member: insert: %w", err)
	}
	return GroupMember{
		GroupURN:  grpURN,
		MemberURN: memberURN,
		Role:      role,
		JoinedAt:  now,
	}, nil
}

// RemoveMember removes memberURN from grpURN. byURN must be owner or
// moderator (D8). The group's owner cannot be removed by RemoveMember
// — the owner must use LeaveGroup after transferring ownership.
//
// Errors:
//   - ErrInvalidRequest — bad URN, not a group URN.
//   - ErrNotFound       — group not in registry; member not in group.
//   - ErrForbidden      — byURN not owner/moderator; attempting to remove an owner.
func (s *Service) RemoveMember(ctx context.Context, grpURN, memberURN, byURN string) error {
	if grpURN == "" || memberURN == "" || byURN == "" {
		return fmt.Errorf("registry: remove member: %w: grpURN + memberURN + byURN required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return fmt.Errorf("registry: remove member: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	if _, err := s.storage.GetProfile(ctx, grpURN); err != nil {
		return err
	}
	byRole, ok, err := s.storage.GroupMemberRole(ctx, grpURN, byURN)
	if err != nil {
		return fmt.Errorf("registry: remove member: by-role check: %w", err)
	}
	if !ok || (byRole != MemberRoleOwner && byRole != MemberRoleModerator) {
		return fmt.Errorf("registry: remove member: %w: byURN must be owner or moderator", ErrForbidden)
	}
	targetRole, present, err := s.storage.GroupMemberRole(ctx, grpURN, memberURN)
	if err != nil {
		return fmt.Errorf("registry: remove member: target-role check: %w", err)
	}
	if !present {
		return fmt.Errorf("registry: remove member: %w: memberURN %q not in group", ErrNotFound, memberURN)
	}
	if targetRole == MemberRoleOwner {
		return fmt.Errorf("registry: remove member: %w: cannot remove owner — owner must LeaveGroup after transferring ownership", ErrForbidden)
	}
	return s.storage.RemoveGroupMember(ctx, grpURN, memberURN)
}

// LeaveGroup is the self-remove path. If the leaver is the owner and no
// other owner/moderator exists, the leave is refused — the caller must
// SetMemberRole(other, 'owner') first or ArchiveGroup. byURN is the
// leaver (must equal memberURN in v1 — this is a self-action).
//
// Errors:
//   - ErrInvalidRequest — bad URN, byURN ≠ memberURN (not self), not a group URN.
//   - ErrNotFound       — group not in registry; member not in group.
//   - ErrForbidden      — owner leaving without a moderator successor.
func (s *Service) LeaveGroup(ctx context.Context, grpURN, memberURN string) error {
	if grpURN == "" || memberURN == "" {
		return fmt.Errorf("registry: leave group: %w: grpURN + memberURN required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return fmt.Errorf("registry: leave group: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	if _, err := s.storage.GetProfile(ctx, grpURN); err != nil {
		return err
	}
	role, present, err := s.storage.GroupMemberRole(ctx, grpURN, memberURN)
	if err != nil {
		return fmt.Errorf("registry: leave group: role check: %w", err)
	}
	if !present {
		return fmt.Errorf("registry: leave group: %w: memberURN %q not in group", ErrNotFound, memberURN)
	}
	if role == MemberRoleOwner {
		others, err := s.storage.CountModeratorsExcluding(ctx, grpURN, memberURN)
		if err != nil {
			return fmt.Errorf("registry: leave group: moderator count: %w", err)
		}
		if others == 0 {
			return fmt.Errorf("registry: leave group: %w: cannot_leave_without_owner_transfer — promote another member to owner/moderator first or ArchiveGroup", ErrForbidden)
		}
	}
	return s.storage.RemoveGroupMember(ctx, grpURN, memberURN)
}

// SetMemberRole changes memberURN's role within grpURN. Promotion to
// owner is restricted to owner-only-by (transfers ownership). Promotion
// to moderator may be done by owner or moderator. byURN itself must be
// owner or moderator.
//
// Errors:
//   - ErrInvalidRequest — bad URN, invalid role, missing args.
//   - ErrNotFound       — group not in registry; member not in group.
//   - ErrForbidden      — byURN not authorized for this transition.
func (s *Service) SetMemberRole(ctx context.Context, grpURN, memberURN string, role MemberRole, byURN string) error {
	if grpURN == "" || memberURN == "" || byURN == "" {
		return fmt.Errorf("registry: set member role: %w: grpURN + memberURN + byURN required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return fmt.Errorf("registry: set member role: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	switch role {
	case MemberRoleMember, MemberRoleModerator, MemberRoleOwner:
	default:
		return fmt.Errorf("registry: set member role: %w: invalid role %q", ErrInvalidRequest, role)
	}
	if _, err := s.storage.GetProfile(ctx, grpURN); err != nil {
		return err
	}
	byRole, ok, err := s.storage.GroupMemberRole(ctx, grpURN, byURN)
	if err != nil {
		return fmt.Errorf("registry: set member role: by-role check: %w", err)
	}
	if !ok || (byRole != MemberRoleOwner && byRole != MemberRoleModerator) {
		return fmt.Errorf("registry: set member role: %w: byURN must be owner or moderator", ErrForbidden)
	}
	// Promotion to owner is owner-only. Moderators can promote to moderator
	// but NOT to owner.
	if role == MemberRoleOwner && byRole != MemberRoleOwner {
		return fmt.Errorf("registry: set member role: %w: only an owner can promote to owner (transfers ownership)", ErrForbidden)
	}
	if _, present, err := s.storage.GroupMemberRole(ctx, grpURN, memberURN); err != nil {
		return fmt.Errorf("registry: set member role: target-role check: %w", err)
	} else if !present {
		return fmt.Errorf("registry: set member role: %w: memberURN %q not in group", ErrNotFound, memberURN)
	}
	return s.storage.UpdateGroupMemberRole(ctx, grpURN, memberURN, role)
}

// ListMembers returns the members of grpURN, ordered by joined_at ASC,
// with display_name hydrated from registry_entries. v1 has no access
// control on this call — member lists are visible to all members per
// the sprint's review notes. If/when private membership lands, this is
// where the access check goes.
//
// Errors:
//   - ErrInvalidRequest — empty/malformed grpURN, not a group URN.
//   - ErrNotFound       — grpURN not in registry.
func (s *Service) ListMembers(ctx context.Context, grpURN string) ([]GroupMember, error) {
	if grpURN == "" {
		return nil, fmt.Errorf("registry: list members: %w: grpURN required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return nil, fmt.Errorf("registry: list members: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	if _, err := s.storage.GetProfile(ctx, grpURN); err != nil {
		return nil, err
	}
	out, err := s.storage.ListMembers(ctx, grpURN)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []GroupMember{}
	}
	return out, nil
}

// ─── group messaging (v060-05 T-04) ──────────────────────────────────────────

// ErrGroupArchived is returned by SendToGroup when the target group's
// status is StatusArchived. HTTP layer maps to 423 Locked per sprint.
var ErrGroupArchived = errors.New("registry: group is archived (read-only)")

// SendToGroup writes a message addressed to grpURN. fromURN must be a
// current member of the group; the group must not be archived. After
// the row is committed, the optional MentionParser hook scans the
// payload for `@<urn>` patterns and emits notice envelopes — failures
// are logged in the parser (fire-and-forget per D11/D12) and never
// fail SendToGroup.
//
// Errors:
//   - ErrInvalidRequest — bad URN, not a group URN, missing args.
//   - ErrNotFound       — group not in registry.
//   - ErrForbidden      — fromURN not a member.
//   - ErrGroupArchived  — group status is archived.
func (s *Service) SendToGroup(ctx context.Context, grpURN, fromURN, kind, threadID, contentType string, payload json.RawMessage) (GroupMessage, error) {
	if grpURN == "" || fromURN == "" || kind == "" {
		return GroupMessage{}, fmt.Errorf("registry: send to group: %w: grpURN + fromURN + kind required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return GroupMessage{}, fmt.Errorf("registry: send to group: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	existing, err := s.storage.GetProfile(ctx, grpURN)
	if err != nil {
		return GroupMessage{}, err
	}
	if existing.Status == StatusArchived {
		return GroupMessage{}, fmt.Errorf("registry: send to group %q: %w", grpURN, ErrGroupArchived)
	}
	if _, present, err := s.storage.GroupMemberRole(ctx, grpURN, fromURN); err != nil {
		return GroupMessage{}, fmt.Errorf("registry: send to group: member check: %w", err)
	} else if !present {
		return GroupMessage{}, fmt.Errorf("registry: send to group: %w: fromURN %q is not a member of %q", ErrForbidden, fromURN, grpURN)
	}
	// Pre-commit mention validation. An ErrAmbiguousMention here aborts
	// the send (HTTP 400 per sprint). Other parser errors abort too —
	// callers can errors.Is to distinguish.
	var mentions []Mention
	if s.mentionParser != nil {
		m, err := s.mentionParser.Parse(ctx, payload, grpURN)
		if err != nil {
			return GroupMessage{}, fmt.Errorf("registry: send to group: %w", err)
		}
		mentions = m
	}
	gm, err := s.storage.InsertGroupMessage(ctx, grpURN, fromURN, kind, threadID, contentType, payload)
	if err != nil {
		return GroupMessage{}, err
	}
	// Post-commit mention dispatch — fire-and-forget per D11/D12. Parser
	// errors are advisory; we don't roll back the group message.
	if s.mentionParser != nil && len(mentions) > 0 {
		s.mentionParser.Dispatch(ctx, gm, mentions)
	}
	// T04: post-commit, best-effort durable fanout. The room body above is
	// already committed and is SendToGroup's real contract; see
	// group_fanout.go for why this never rolls back or fails the send.
	s.sendToGroupWithFanout(ctx, gm)
	return gm, nil
}

// ListGroupMessages returns messages addressed to grpURN with group_seq
// > sinceSeq. sinceSeq=0 means "from the member's last_read_seq" —
// callers who explicitly want full visible history pass a negative
// sentinel? No: 0 is the default, and the member's last_read_seq is
// substituted if the caller passes 0. To get pre-cursor history pass
// the explicit lower bound.
//
// The result is gated by the member's joined_at (no pre-membership
// history). This call does NOT bump the read cursor — MarkRead is
// dedicated to that.
//
// Errors:
//   - ErrInvalidRequest — bad URN, missing args.
//   - ErrNotFound       — group not in registry.
//   - ErrForbidden      — memberURN not a member.
func (s *Service) ListGroupMessages(ctx context.Context, grpURN, memberURN string, sinceSeq int64, threadID string, limit int) ([]GroupMessage, error) {
	if grpURN == "" || memberURN == "" {
		return nil, fmt.Errorf("registry: list group messages: %w: grpURN + memberURN required", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return nil, fmt.Errorf("registry: list group messages: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	if _, err := s.storage.GetProfile(ctx, grpURN); err != nil {
		return nil, err
	}
	joinedAt, present, err := s.storage.GroupMemberJoinedAt(ctx, grpURN, memberURN)
	if err != nil {
		return nil, fmt.Errorf("registry: list group messages: membership check: %w", err)
	}
	if !present {
		return nil, fmt.Errorf("registry: list group messages: %w: memberURN %q not in group", ErrForbidden, memberURN)
	}
	if sinceSeq == 0 {
		cursor, _, err := s.storage.GroupMemberLastReadSeq(ctx, grpURN, memberURN)
		if err != nil {
			return nil, fmt.Errorf("registry: list group messages: cursor: %w", err)
		}
		sinceSeq = cursor
	}
	out, err := s.storage.ListGroupMessages(ctx, grpURN, sinceSeq, threadID, limit, joinedAt)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []GroupMessage{}
	}
	return out, nil
}

// MarkRead sets the member's last_read_seq to max(last_read_seq, upToSeq).
// Idempotent / monotonic — smaller upToSeq is a no-op.
//
// Errors:
//   - ErrInvalidRequest — bad URN, missing args, negative upToSeq.
//   - ErrNotFound       — group not in registry; member not in group.
func (s *Service) MarkRead(ctx context.Context, grpURN, memberURN string, upToSeq int64) error {
	if grpURN == "" || memberURN == "" {
		return fmt.Errorf("registry: mark read: %w: grpURN + memberURN required", ErrInvalidRequest)
	}
	if upToSeq < 0 {
		return fmt.Errorf("registry: mark read: %w: upToSeq must be ≥ 0", ErrInvalidRequest)
	}
	if !IsGroupURN(grpURN) {
		return fmt.Errorf("registry: mark read: %w: not a group URN: %q", ErrInvalidRequest, grpURN)
	}
	if _, err := s.storage.GetProfile(ctx, grpURN); err != nil {
		return err
	}
	return s.storage.BumpGroupReadCursor(ctx, grpURN, memberURN, upToSeq)
}

// GetMyMentions returns the notice envelopes addressed to memberURN
// whose payload carries a `group` key (set by T-05's parser). Convenience
// wrapper over storage.ListMentionsForMember — saves callers from
// constructing the json_extract filter themselves.
func (s *Service) GetMyMentions(ctx context.Context, memberURN string, sinceTS time.Time, limit int) ([]GroupMessage, error) {
	if memberURN == "" {
		return nil, fmt.Errorf("registry: get my mentions: %w: memberURN required", ErrInvalidRequest)
	}
	if _, err := ParseRegistryURN(memberURN); err != nil {
		return nil, fmt.Errorf("registry: get my mentions: %w: invalid URN: %w", ErrInvalidRequest, err)
	}
	out, err := s.storage.ListMentionsForMember(ctx, memberURN, sinceTS, limit)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []GroupMessage{}
	}
	return out, nil
}

// Lookup returns the Profile for urn. ErrNotFound propagates verbatim so
// callers can errors.Is it.
func (s *Service) Lookup(ctx context.Context, urn string) (Profile, error) {
	return s.storage.GetProfile(ctx, urn)
}

// LookupExternalIDsForURN returns every external-id attachment recorded
// for urn (T08: exposed publicly for the self-discovery/whoami surface;
// previously storage-internal only, used by Merge/AttachExternalID).
func (s *Service) LookupExternalIDsForURN(ctx context.Context, urn string) ([]ExternalID, error) {
	if urn == "" {
		return nil, fmt.Errorf("registry: lookup external ids: %w: urn required", ErrInvalidRequest)
	}
	return s.storage.LookupExternalIDsForURN(ctx, urn)
}

// LookupBy resolves one substrate-local identifier to its registry profile.
// When substrate is empty, the earliest-attached match across every substrate
// wins.
func (s *Service) LookupBy(ctx context.Context, kind Kind, externalID, substrate string) (Profile, error) {
	switch kind {
	case KindAgent, KindProject, KindGroup:
	default:
		return Profile{}, fmt.Errorf("registry: lookup by: %w: unsupported kind %q", ErrInvalidRequest, string(kind))
	}
	if externalID == "" {
		return Profile{}, fmt.Errorf("registry: lookup by: %w: external_id required", ErrInvalidRequest)
	}
	urn, ok, err := s.storage.LookupURNByExternalID(ctx, kind, externalID, substrate)
	if err != nil {
		return Profile{}, err
	}
	if !ok {
		return Profile{}, ErrNotFound
	}
	return s.storage.GetProfile(ctx, urn)
}

// AttachExternalID validates and records one substrate-local identifier for an
// existing URN.
func (s *Service) AttachExternalID(ctx context.Context, urn, substrate, externalID string) error {
	if urn == "" || substrate == "" || externalID == "" {
		return fmt.Errorf("registry: attach external id: %w: urn + substrate + external_id required", ErrInvalidRequest)
	}
	profile, err := s.storage.GetProfile(ctx, urn)
	if err != nil {
		return err
	}
	if existing, ok, err := s.storage.LookupURNByExternalID(ctx, profile.Kind, externalID, substrate); err != nil {
		return err
	} else if ok {
		if existing == urn {
			return nil
		}
		return fmt.Errorf("registry: attach external id: %w: substrate %q external_id %q already attached to %s", ErrInvalidRequest, substrate, externalID, existing)
	}
	return s.storage.AttachExternalID(ctx, urn, substrate, externalID)
}

// Merge consolidates urnSrc into urnDst. Destination wins scalar conflicts;
// array-shaped fields and external IDs are unioned onto the destination.
func (s *Service) Merge(ctx context.Context, urnSrc, urnDst string) (Profile, error) {
	if urnSrc == "" || urnDst == "" {
		return Profile{}, fmt.Errorf("registry: merge: %w: urn-src + urn-dst required", ErrInvalidRequest)
	}
	if urnSrc == urnDst {
		return Profile{}, fmt.Errorf("registry: merge: %w: urn-src and urn-dst must differ", ErrInvalidRequest)
	}
	src, err := s.storage.GetProfile(ctx, urnSrc)
	if err != nil {
		return Profile{}, err
	}
	dst, err := s.storage.GetProfile(ctx, urnDst)
	if err != nil {
		return Profile{}, err
	}
	if src.Kind != dst.Kind {
		return Profile{}, fmt.Errorf("registry: merge: %w: kind mismatch %q != %q", ErrInvalidRequest, src.Kind, dst.Kind)
	}

	for _, ext := range src.ExternalIDs {
		if _, ok := dst.ExternalIDFor(ext.Substrate); ok {
			continue
		}
		if err := s.storage.DetachExternalID(ctx, urnSrc, ext.Substrate); err != nil {
			return Profile{}, err
		}
		if err := s.storage.AttachExternalID(ctx, urnDst, ext.Substrate, ext.ExternalID); err != nil {
			return Profile{}, err
		}
	}

	patch := UpdatePatch{
		LastUpdatedBy: "system:merge",
		Capabilities: &ArrayPatch[string]{
			Mode:  ArrayModeAppend,
			Value: src.Capabilities,
		},
		Skills: &ArrayPatch[Skill]{
			Mode:  ArrayModeAppend,
			Value: src.Skills,
		},
		Links: &ArrayPatch[Link]{
			Mode:  ArrayModeAppend,
			Value: src.Links,
		},
	}
	if mergedMeta, ok := mergeKindMeta(dst.KindMeta, src.KindMeta); ok {
		patch.KindMeta = mergedMeta
	}
	if _, err := s.UpdateSelf(ctx, urnDst, patch); err != nil {
		return Profile{}, err
	}
	if err := s.storage.UpdateProfileFields(ctx, urnSrc, map[string]any{
		"status":          string(StatusMerged),
		"merged_into":     urnDst,
		"last_updated_by": "system:merge",
	}); err != nil {
		return Profile{}, err
	}
	out, err := s.storage.GetProfile(ctx, urnDst)
	if err != nil {
		return Profile{}, err
	}
	out.ExternalIDs = dedupExternalIDs(out.ExternalIDs)
	return out, nil
}

func mergeKindMeta(dstRaw, srcRaw json.RawMessage) (json.RawMessage, bool) {
	if len(dstRaw) == 0 && len(srcRaw) == 0 {
		return nil, false
	}
	if len(srcRaw) == 0 {
		return dstRaw, len(dstRaw) > 0
	}
	if len(dstRaw) == 0 {
		return srcRaw, true
	}
	var dst map[string]any
	if err := json.Unmarshal(dstRaw, &dst); err != nil {
		return dstRaw, true
	}
	var src map[string]any
	if err := json.Unmarshal(srcRaw, &src); err != nil {
		return dstRaw, true
	}
	for k, v := range src {
		if _, exists := dst[k]; exists {
			continue
		}
		dst[k] = v
	}
	merged, err := json.Marshal(dst)
	if err != nil {
		return dstRaw, true
	}
	return json.RawMessage(merged), true
}

func dedupExternalIDs(in []ExternalID) []ExternalID {
	seen := map[string]struct{}{}
	out := make([]ExternalID, 0, len(in))
	for _, ext := range in {
		key := ext.Substrate + "\x00" + ext.ExternalID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ext)
	}
	slices.SortFunc(out, func(a, b ExternalID) int {
		if a.Substrate < b.Substrate {
			return -1
		}
		if a.Substrate > b.Substrate {
			return 1
		}
		if a.ExternalID < b.ExternalID {
			return -1
		}
		if a.ExternalID > b.ExternalID {
			return 1
		}
		return 0
	})
	return out
}

// FindByDisplayName returns the profiles whose display_name equals name.
// Exposes the storage lookup so the v060-05 mention parser
// (messaging.Lookup interface) can resolve short-form `@<display_name>`
// tokens against the live registry. Returns a non-nil zero-length slice
// when nothing matches; ErrInvalidRequest when name is empty.
func (s *Service) FindByDisplayName(ctx context.Context, name string) ([]Profile, error) {
	if name == "" {
		return nil, fmt.Errorf("registry: find by display name: %w: name required", ErrInvalidRequest)
	}
	out, err := s.storage.FindByDisplayName(ctx, name)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Profile{}
	}
	return out, nil
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
