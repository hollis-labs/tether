package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Kind is the registry-entry kind. v060-01 ships agent + project; future
// kinds extend via Register without schema change.
type Kind string

const (
	KindAgent   Kind = "agent"
	KindProject Kind = "project"
	KindGroup   Kind = "group"
)

// Status is the lifecycle state. Soft-deleted rows carry StatusDeprecated
// (D11) and remain readable by URN but are excluded from default Search.
type Status string

const (
	StatusActive     Status = "active"
	StatusDeprecated Status = "deprecated"
	// StatusArchived is the group-archive sentinel (v060-05 D9). A group
	// in status='archived' is read-only — members can still ListGroupMessages
	// but SendToGroup returns 423 locked.
	StatusArchived Status = "archived"
)

// Profile is the public-identity projection of a registry row. Profile is
// BOTH the storage-layer row mirror and the HTTP/MCP API envelope — the
// two stores share this shape. The owning substrate's operational config
// lives behind Callback, not in Profile (D1 two-store model).
type Profile struct {
	URN           string          `json:"urn"`
	Kind          Kind            `json:"kind"`
	MuxInstanceID string          `json:"mux_instance_id"`
	DisplayName   string          `json:"display_name"`
	Title         string          `json:"title,omitempty"`
	Role          string          `json:"role,omitempty"`
	Description   string          `json:"description,omitempty"`
	Avatar        string          `json:"avatar,omitempty"`
	Project       string          `json:"project,omitempty"`
	Status        Status          `json:"status"`
	Callback      *Callback       `json:"callback,omitempty"`
	CachedAt      *time.Time      `json:"cached_at,omitempty"`
	HealthStatus  string          `json:"health_status,omitempty"`
	LastSeenAt    *time.Time      `json:"last_seen_at,omitempty"`
	HostAddress   string          `json:"host_address,omitempty"`
	KindMeta      json.RawMessage `json:"kind_meta,omitempty"`
	LastUpdatedBy string          `json:"last_updated_by,omitempty"`
	Capabilities  []string        `json:"capabilities,omitempty"`
	Skills        []Skill         `json:"skills,omitempty"`
	Links         []Link          `json:"links,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// Skill carries the D13 shape: name + learned_at are required; via + level
// are optional (level is free-form v1).
type Skill struct {
	Name      string    `json:"name"`
	LearnedAt time.Time `json:"learned_at"`
	Via       string    `json:"via,omitempty"`
	Level     string    `json:"level,omitempty"`
}

// Link is a free-form-kind relationship (D16). The kind vocabulary is
// blessed in ADR 0041; unknown kinds work without schema change.
type Link struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// Callback identifies the owning substrate's ops-store endpoint that Sync
// uses to refresh thin-profile columns. Scheme dispatches to a Resolver;
// Target is the full URI handed to the resolver. file:// + cli:// in v1
// (D9); http:// + mcp:// land in v060-02.
type Callback struct {
	Scheme string `json:"scheme"`
	Target string `json:"target"`
}

// Filter narrows a Search. All fields are optional and combine with AND
// (D6). Empty Status returns active rows only by default; pass
// StatusDeprecated to include soft-deleted rows.
type Filter struct {
	Role       string
	Title      string
	Project    string
	Capability string
	SkillName  string
	Status     string
}

// UpdatePatch is the input shape for Service.UpdateSelf. Scalar pointer
// fields are nil = no change, non-nil = set (including pointer-to-empty-
// string = explicit clear). Array patches are nil = no change; the inner
// ArrayPatch carries the merge mode. KindMeta is replace-on-present (no
// shallow merge in v1). LastUpdatedBy is required.
type UpdatePatch struct {
	DisplayName   *string             `json:"display_name,omitempty"`
	Title         *string             `json:"title,omitempty"`
	Role          *string             `json:"role,omitempty"`
	Description   *string             `json:"description,omitempty"`
	Avatar        *string             `json:"avatar,omitempty"`
	Project       *string             `json:"project,omitempty"`
	Status        *Status             `json:"status,omitempty"`
	HealthStatus  *string             `json:"health_status,omitempty"` // D15
	LastSeenAt    *time.Time          `json:"last_seen_at,omitempty"`  // D15
	HostAddress   *string             `json:"host_address,omitempty"`  // D15
	KindMeta      json.RawMessage     `json:"kind_meta,omitempty"`
	LastUpdatedBy string              `json:"last_updated_by"` // required
	Capabilities  *ArrayPatch[string] `json:"capabilities,omitempty"`
	Skills        *ArrayPatch[Skill]  `json:"skills,omitempty"`
	Links         *ArrayPatch[Link]   `json:"links,omitempty"`
}

// MemberRole is the role of a member inside a group (v060-05 D8).
// Owner is the creator (or whoever ownership was transferred to);
// moderator can invite/kick; member can post and read. Stored in
// group_members.role with a CHECK constraint matching this enum.
type MemberRole string

const (
	MemberRoleMember    MemberRole = "member"
	MemberRoleModerator MemberRole = "moderator"
	MemberRoleOwner     MemberRole = "owner"
)

// GroupMember is a row from the group_members sibling table (v060-05 D5).
// Membership carries per-member state (role + read cursor) which is why
// it cannot live as a registry_links row.
type GroupMember struct {
	GroupURN    string     `json:"group_urn"`
	MemberURN   string     `json:"member_urn"`
	Role        MemberRole `json:"role"`
	JoinedAt    time.Time  `json:"joined_at"`
	LastReadSeq int64      `json:"last_read_seq"`
	// DisplayName is hydrated by ListMembers (T-03) via a JOIN against
	// registry_entries; not stored in group_members itself.
	DisplayName string `json:"display_name,omitempty"`
}

// ArrayMode controls how an UpdateSelf array patch merges into the
// existing row (D5).
type ArrayMode string

const (
	ArrayModeReplace ArrayMode = "replace"
	ArrayModeAppend  ArrayMode = "append"
	ArrayModeRemove  ArrayMode = "remove"
)

// ArrayPatch is the merge-mode wrapper for array fields on UpdateSelf
// (D5). Two wire shapes are accepted on input:
//
//   - Shorthand `[...]` — equivalent to ArrayModeReplace.
//   - Explicit `{"mode":"append"|"replace"|"remove","value":[...]}`.
//
// Output always uses the explicit form. Remove matches: capabilities by
// string equality, skills by Name, links by (Kind, Target).
type ArrayPatch[T any] struct {
	Mode  ArrayMode `json:"mode"`
	Value []T       `json:"value"`
}

// UnmarshalJSON accepts both the shorthand and explicit wire shapes.
// encoding/json may pass the raw token with leading/trailing whitespace,
// so we trim before inspecting the first byte (otherwise shorthand `  [...]`
// would mis-detect as the object form).
func (p *ArrayPatch[T]) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("registry: empty array patch")
	}
	if trimmed[0] == '[' {
		var arr []T
		if err := json.Unmarshal(data, &arr); err != nil {
			return fmt.Errorf("registry: array patch shorthand: %w", err)
		}
		p.Mode = ArrayModeReplace
		p.Value = arr
		return nil
	}
	var obj struct {
		Mode  ArrayMode `json:"mode"`
		Value []T       `json:"value"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("registry: array patch object: %w", err)
	}
	switch obj.Mode {
	case ArrayModeReplace, ArrayModeAppend, ArrayModeRemove:
	default:
		return fmt.Errorf("registry: invalid array patch mode %q", obj.Mode)
	}
	p.Mode = obj.Mode
	p.Value = obj.Value
	return nil
}
