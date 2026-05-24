package registry

// storage.go — SQLite-backed CRUD + Search for the federation directory
// service. Pure data access: no merge semantics, no URN minting, no
// validation beyond the column whitelist on UpdateProfileFields. Service
// logic lives in service.go (T-v060-01-03).
//
// Transaction discipline. The single-connection pool (MaxOpenConns=1 in
// internal/store/sqlite.go) serializes all access, so the default deferred
// BeginTx is sufficient — IMMEDIATE locks are not required.
//
// SoftDelete (D11) flips status='deprecated' on registry_entries and leaves
// the three child tables (capabilities, skills, links) UNTOUCHED. v1 has no
// hard-delete; cascade-equivalent cleanup is a Go-layer concern in a later
// sprint.
//
// Search defaults exclude soft-deleted rows. Filter.Status semantics:
//   - "" (empty) → status='active' only
//   - "deprecated" → status='deprecated' only
//   - StatusAny ("*") → all statuses (sentinel; documented below)

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned by GetProfile when no row matches the given URN.
// Callers use errors.Is to detect it.
var ErrNotFound = errors.New("registry: not found")

// ErrUnknownColumn is returned by UpdateProfileFields when the fields map
// references a column outside the whitelist. Callers use errors.Is.
var ErrUnknownColumn = errors.New("registry: unknown column in UpdateProfileFields")

// StatusAny is the Search Filter.Status sentinel for "return rows in any
// status". Empty Filter.Status defaults to active-only; "deprecated"
// returns deprecated only; StatusAny disables the status filter entirely.
const StatusAny = "*"

// defaultMuxInstanceID matches the column DEFAULT in 0015_registry.sql.
const defaultMuxInstanceID = "agent-mux"

// updateProfileFieldAllowlist is the column whitelist for
// UpdateProfileFields. Immutable columns (urn, kind, mux_instance_id,
// created_at, updated_at) are intentionally excluded — updated_at is
// always bumped automatically; the others are write-once on insert.
var updateProfileFieldAllowlist = map[string]struct{}{
	"display_name":    {},
	"title":           {},
	"role":            {},
	"description":     {},
	"avatar":          {},
	"project":         {},
	"status":          {},
	"callback_json":   {},
	"cached_at":       {},
	"health_status":   {},
	"last_seen_at":    {},
	"host_address":    {},
	"merged_into":     {},
	"kind_meta_json":  {},
	"last_updated_by": {},
}

// Storage is a thin database/sql wrapper that owns the four registry_*
// tables. It holds no state beyond the *sql.DB handle.
type Storage struct {
	db *sql.DB
}

// NewStorage returns a Storage bound to db. The database must already have
// migration 0015_registry.sql applied.
func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

// ─── Insert / Get ────────────────────────────────────────────────────────────

// InsertProfile writes a Profile and all its child rows in a single
// transaction. CreatedAt/UpdatedAt default to time.Now().UTC() when zero;
// Status defaults to StatusActive when empty; MuxInstanceID defaults to
// "agent-mux" when empty. Callback is JSON-marshaled for callback_json;
// KindMeta (json.RawMessage) is written verbatim to kind_meta_json.
func (s *Storage) InsertProfile(ctx context.Context, p Profile) error {
	if p.URN == "" {
		return errors.New("registry: insert profile: urn required")
	}
	if p.DisplayName == "" {
		return errors.New("registry: insert profile: display_name required")
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = now
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.MuxInstanceID == "" {
		p.MuxInstanceID = defaultMuxInstanceID
	}

	var callbackJSON sql.NullString
	if p.Callback != nil {
		b, err := json.Marshal(p.Callback)
		if err != nil {
			return fmt.Errorf("registry: marshal callback: %w", err)
		}
		callbackJSON = sql.NullString{String: string(b), Valid: true}
	}
	var kindMetaJSON sql.NullString
	if len(p.KindMeta) > 0 {
		kindMetaJSON = sql.NullString{String: string(p.KindMeta), Valid: true}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("registry: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO registry_entries
		    (urn, kind, mux_instance_id, display_name, title, role, description,
		     avatar, project, status, callback_json, cached_at, health_status,
		     last_seen_at, host_address, merged_into, kind_meta_json, last_updated_by,
		     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.URN, string(p.Kind), p.MuxInstanceID, p.DisplayName,
		nullIfEmpty(p.Title), nullIfEmpty(p.Role), nullIfEmpty(p.Description),
		nullIfEmpty(p.Avatar), nullIfEmpty(p.Project), string(p.Status),
		callbackJSON, nullIfTimePtr(p.CachedAt), nullIfEmpty(p.HealthStatus),
		nullIfTimePtr(p.LastSeenAt), nullIfEmpty(p.HostAddress), nullIfEmpty(p.MergedInto),
		kindMetaJSON, nullIfEmpty(p.LastUpdatedBy),
		formatTime(p.CreatedAt), formatTime(p.UpdatedAt),
	); err != nil {
		return fmt.Errorf("registry: insert entry: %w", err)
	}

	for _, c := range p.Capabilities {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_capabilities (urn, capability) VALUES (?, ?)`,
			p.URN, c); err != nil {
			return fmt.Errorf("registry: insert capability %q: %w", c, err)
		}
	}
	for _, sk := range p.Skills {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_skills (urn, name, learned_at, via, level)
			 VALUES (?, ?, ?, ?, ?)`,
			p.URN, sk.Name, formatTime(sk.LearnedAt),
			nullIfEmpty(sk.Via), nullIfEmpty(sk.Level)); err != nil {
			return fmt.Errorf("registry: insert skill %q: %w", sk.Name, err)
		}
	}
	for _, l := range p.Links {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_links (urn, kind, target) VALUES (?, ?, ?)`,
			p.URN, l.Kind, l.Target); err != nil {
			return fmt.Errorf("registry: insert link (%s,%s): %w", l.Kind, l.Target, err)
		}
	}
	for _, ext := range p.ExternalIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_external_ids (urn, substrate, external_id, attached_at)
			 VALUES (?, ?, ?, ?)`,
			p.URN, ext.Substrate, ext.ExternalID, formatTime(ext.AttachedAt)); err != nil {
			return fmt.Errorf("registry: insert external_id (%s,%s): %w", ext.Substrate, ext.ExternalID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: commit insert: %w", err)
	}
	return nil
}

// GetProfile returns the Profile for urn. Returns a wrapped ErrNotFound
// when the entry row is absent. Capabilities/skills/links are fetched in
// three follow-up queries against the same *sql.DB (the single-connection
// pool serializes them).
func (s *Storage) GetProfile(ctx context.Context, urn string) (Profile, error) {
	if urn == "" {
		return Profile{}, errors.New("registry: get profile: urn required")
	}
	p, err := s.selectEntry(ctx, urn)
	if err != nil {
		return Profile{}, err
	}
	caps, err := s.selectCapabilities(ctx, urn)
	if err != nil {
		return Profile{}, err
	}
	skills, err := s.selectSkills(ctx, urn)
	if err != nil {
		return Profile{}, err
	}
	links, err := s.selectLinks(ctx, urn)
	if err != nil {
		return Profile{}, err
	}
	externalIDs, err := s.selectExternalIDs(ctx, urn)
	if err != nil {
		return Profile{}, err
	}
	p.Capabilities = caps
	p.Skills = skills
	p.Links = links
	p.ExternalIDs = externalIDs
	return p, nil
}

// URNExists reports whether a row with the given URN is present. Satisfies
// the URNExistsFunc signature from id.go.
func (s *Storage) URNExists(ctx context.Context, urn string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM registry_entries WHERE urn = ? LIMIT 1`, urn,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("registry: urn exists: %w", err)
	}
	return true, nil
}

// FindByCallbackTarget returns the Profile whose callback_json.target
// equals target exactly, or ErrNotFound when no row matches. Bootstrap's
// idempotency lookup uses this seam: the canonical "have I imported this
// source file before?" check matches on the row's callback target rather
// than threading a separate source_path field through kind_meta (the
// callback already carries the abs path as file://<path>).
//
// SQL relies on SQLite's json1 extension's json_extract() — modernc.org/
// sqlite ships json1 built in (verified at v060-01 T-08). If the dialect
// ever changes, fall back to iterating Search(...) with StatusAny and
// matching in Go; the contract here is just "first row with this exact
// callback.target". Broader cross-substrate dedup primitives (LookupBy
// with external_id + substrate attribution) land in v060-02.
//
// Match semantics: exact string equality on the JSON-extracted target.
// Rows with no callback (NULL callback_json) are excluded; soft-deleted
// rows are returned (callers decide whether to refresh or skip).
func (s *Storage) FindByCallbackTarget(ctx context.Context, target string) (Profile, error) {
	if target == "" {
		return Profile{}, errors.New("registry: find by callback target: target required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT urn, kind, mux_instance_id, display_name, title, role, description,
		        avatar, project, status, callback_json, cached_at, health_status,
		        last_seen_at, host_address, merged_into, kind_meta_json, last_updated_by,
		        created_at, updated_at
		   FROM registry_entries
		  WHERE callback_json IS NOT NULL
		    AND json_extract(callback_json, '$.target') = ?
		  LIMIT 1`, target)
	p, err := scanEntryRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, fmt.Errorf("registry: find by callback target %q: %w", target, ErrNotFound)
	}
	if err != nil {
		return Profile{}, err
	}
	// Hydrate child rows so the returned Profile mirrors GetProfile's shape.
	caps, err := s.selectCapabilities(ctx, p.URN)
	if err != nil {
		return Profile{}, err
	}
	skills, err := s.selectSkills(ctx, p.URN)
	if err != nil {
		return Profile{}, err
	}
	links, err := s.selectLinks(ctx, p.URN)
	if err != nil {
		return Profile{}, err
	}
	externalIDs, err := s.selectExternalIDs(ctx, p.URN)
	if err != nil {
		return Profile{}, err
	}
	p.Capabilities = caps
	p.Skills = skills
	p.Links = links
	p.ExternalIDs = externalIDs
	return p, nil
}

// LookupExternalIDsForURN returns every external-id attachment recorded for
// urn, ordered by attached_at then substrate for stable caller output.
func (s *Storage) LookupExternalIDsForURN(ctx context.Context, urn string) ([]ExternalID, error) {
	if urn == "" {
		return nil, errors.New("registry: lookup external ids: urn required")
	}
	return s.selectExternalIDs(ctx, urn)
}

// LookupURNByExternalID finds the first URN of kind attached to externalID. If
// substrate is non-empty, the lookup is constrained to that substrate;
// otherwise it matches across every substrate in attached_at order.
func (s *Storage) LookupURNByExternalID(ctx context.Context, kind Kind, externalID, substrate string) (string, bool, error) {
	if kind == "" {
		return "", false, errors.New("registry: lookup by external id: kind required")
	}
	if externalID == "" {
		return "", false, errors.New("registry: lookup by external id: external_id required")
	}

	query := `SELECT e.urn
		FROM registry_external_ids x
		JOIN registry_entries e ON e.urn = x.urn
	   WHERE e.kind = ?
	     AND x.external_id = ?`
	args := []any{string(kind), externalID}
	if substrate != "" {
		query += ` AND x.substrate = ?`
		args = append(args, substrate)
	}
	query += ` ORDER BY x.attached_at ASC, x.substrate ASC LIMIT 1`

	var urn string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&urn)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("registry: lookup by external id: %w", err)
	}
	return urn, true, nil
}

// AttachExternalID records one substrate-local identifier for urn.
func (s *Storage) AttachExternalID(ctx context.Context, urn, substrate, externalID string) error {
	if urn == "" {
		return errors.New("registry: attach external id: urn required")
	}
	if substrate == "" {
		return errors.New("registry: attach external id: substrate required")
	}
	if externalID == "" {
		return errors.New("registry: attach external id: external_id required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO registry_external_ids (urn, substrate, external_id, attached_at)
		 VALUES (?, ?, ?, ?)`,
		urn, substrate, externalID, formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("registry: attach external id: %w", err)
	}
	return nil
}

// DetachExternalID removes the substrate attachment for urn. Missing rows are a
// no-op so merge/cleanup callers can treat detach as idempotent.
func (s *Storage) DetachExternalID(ctx context.Context, urn, substrate string) error {
	if urn == "" {
		return errors.New("registry: detach external id: urn required")
	}
	if substrate == "" {
		return errors.New("registry: detach external id: substrate required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM registry_external_ids WHERE urn = ? AND substrate = ?`,
		urn, substrate); err != nil {
		return fmt.Errorf("registry: detach external id: %w", err)
	}
	return nil
}

// ─── Update ──────────────────────────────────────────────────────────────────

// UpdateProfileFields performs a partial column-level update on
// registry_entries only. Keys in fields must be column names from the
// allowlist; any other key returns ErrUnknownColumn. updated_at is always
// bumped in the same statement. Empty fields map is a no-op (no row touch).
func (s *Storage) UpdateProfileFields(ctx context.Context, urn string, fields map[string]any) error {
	if urn == "" {
		return errors.New("registry: update profile: urn required")
	}
	if len(fields) == 0 {
		return nil
	}
	// Stable column order so generated SQL is deterministic across calls.
	cols := make([]string, 0, len(fields))
	for col := range fields {
		if _, ok := updateProfileFieldAllowlist[col]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownColumn, col)
		}
		cols = append(cols, col)
	}
	// Sort the column names so the SQL text is stable (helps test diagnostics).
	sort.Strings(cols)
	// SET clause is built from whitelist-validated column names + literals only;
	// user input only flows into the parameterized values, never the SQL text.
	setClause := ""
	args := make([]any, 0, len(cols)+2)
	for i, col := range cols {
		if i > 0 {
			setClause += ", "
		}
		setClause += col + " = ?"
		args = append(args, fields[col])
	}
	if setClause != "" {
		setClause += ", "
	}
	setClause += "updated_at = ?"
	args = append(args, formatTime(time.Now().UTC()))
	args = append(args, urn)

	// setClause is built from a fixed whitelist of column names + literals;
	// user input only flows into the parameterized args slice, never the SQL text.
	res, err := s.db.ExecContext(ctx,
		`UPDATE registry_entries SET `+setClause+` WHERE urn = ?`,
		args...)
	if err != nil {
		return fmt.Errorf("registry: update profile fields: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("registry: update profile fields %q: %w", urn, ErrNotFound)
	}
	return nil
}

// ─── Replace ─────────────────────────────────────────────────────────────────

// ReplaceCapabilities deletes existing capability rows for urn and inserts
// caps, then bumps the entry's updated_at — all within one transaction.
func (s *Storage) ReplaceCapabilities(ctx context.Context, urn string, caps []string) error {
	return s.replaceChild(ctx, urn, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM registry_capabilities WHERE urn = ?`, urn); err != nil {
			return fmt.Errorf("registry: delete capabilities: %w", err)
		}
		for _, c := range caps {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO registry_capabilities (urn, capability) VALUES (?, ?)`,
				urn, c); err != nil {
				return fmt.Errorf("registry: insert capability %q: %w", c, err)
			}
		}
		return nil
	})
}

// ReplaceSkills mirrors ReplaceCapabilities for the skills table.
func (s *Storage) ReplaceSkills(ctx context.Context, urn string, skills []Skill) error {
	return s.replaceChild(ctx, urn, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM registry_skills WHERE urn = ?`, urn); err != nil {
			return fmt.Errorf("registry: delete skills: %w", err)
		}
		for _, sk := range skills {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO registry_skills (urn, name, learned_at, via, level)
				 VALUES (?, ?, ?, ?, ?)`,
				urn, sk.Name, formatTime(sk.LearnedAt),
				nullIfEmpty(sk.Via), nullIfEmpty(sk.Level)); err != nil {
				return fmt.Errorf("registry: insert skill %q: %w", sk.Name, err)
			}
		}
		return nil
	})
}

// ReplaceLinks mirrors ReplaceCapabilities for the links table.
func (s *Storage) ReplaceLinks(ctx context.Context, urn string, links []Link) error {
	return s.replaceChild(ctx, urn, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM registry_links WHERE urn = ?`, urn); err != nil {
			return fmt.Errorf("registry: delete links: %w", err)
		}
		for _, l := range links {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO registry_links (urn, kind, target) VALUES (?, ?, ?)`,
				urn, l.Kind, l.Target); err != nil {
				return fmt.Errorf("registry: insert link (%s,%s): %w", l.Kind, l.Target, err)
			}
		}
		return nil
	})
}

// ─── Append ──────────────────────────────────────────────────────────────────

// AppendCapabilities inserts each capability with INSERT OR IGNORE so PK
// conflicts are silent dedup. Bumps updated_at. Empty input is a no-op.
func (s *Storage) AppendCapabilities(ctx context.Context, urn string, caps []string) error {
	if len(caps) == 0 {
		return nil
	}
	return s.mutateChild(ctx, urn, func(tx *sql.Tx) error {
		for _, c := range caps {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO registry_capabilities (urn, capability) VALUES (?, ?)`,
				urn, c); err != nil {
				return fmt.Errorf("registry: append capability %q: %w", c, err)
			}
		}
		return nil
	})
}

// AppendSkills uses INSERT OR IGNORE keyed on (urn, name); a re-append of
// an existing skill with a new learned_at does NOT update the existing row
// (that's a ReplaceSkills concern).
func (s *Storage) AppendSkills(ctx context.Context, urn string, skills []Skill) error {
	if len(skills) == 0 {
		return nil
	}
	return s.mutateChild(ctx, urn, func(tx *sql.Tx) error {
		for _, sk := range skills {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO registry_skills (urn, name, learned_at, via, level)
				 VALUES (?, ?, ?, ?, ?)`,
				urn, sk.Name, formatTime(sk.LearnedAt),
				nullIfEmpty(sk.Via), nullIfEmpty(sk.Level)); err != nil {
				return fmt.Errorf("registry: append skill %q: %w", sk.Name, err)
			}
		}
		return nil
	})
}

// AppendLinks uses INSERT OR IGNORE keyed on (urn, kind, target).
func (s *Storage) AppendLinks(ctx context.Context, urn string, links []Link) error {
	if len(links) == 0 {
		return nil
	}
	return s.mutateChild(ctx, urn, func(tx *sql.Tx) error {
		for _, l := range links {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO registry_links (urn, kind, target) VALUES (?, ?, ?)`,
				urn, l.Kind, l.Target); err != nil {
				return fmt.Errorf("registry: append link (%s,%s): %w", l.Kind, l.Target, err)
			}
		}
		return nil
	})
}

// ─── Remove ──────────────────────────────────────────────────────────────────

// RemoveCapabilities deletes the rows matching each capability. Empty
// input is a no-op (no updated_at bump).
func (s *Storage) RemoveCapabilities(ctx context.Context, urn string, caps []string) error {
	if len(caps) == 0 {
		return nil
	}
	return s.mutateChild(ctx, urn, func(tx *sql.Tx) error {
		for _, c := range caps {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM registry_capabilities WHERE urn = ? AND capability = ?`,
				urn, c); err != nil {
				return fmt.Errorf("registry: remove capability %q: %w", c, err)
			}
		}
		return nil
	})
}

// RemoveSkills deletes rows matched by name. Empty input is a no-op.
func (s *Storage) RemoveSkills(ctx context.Context, urn string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	return s.mutateChild(ctx, urn, func(tx *sql.Tx) error {
		for _, n := range names {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM registry_skills WHERE urn = ? AND name = ?`,
				urn, n); err != nil {
				return fmt.Errorf("registry: remove skill %q: %w", n, err)
			}
		}
		return nil
	})
}

// RemoveLinks deletes rows matched by (kind, target). Empty input is a no-op.
func (s *Storage) RemoveLinks(ctx context.Context, urn string, links []Link) error {
	if len(links) == 0 {
		return nil
	}
	return s.mutateChild(ctx, urn, func(tx *sql.Tx) error {
		for _, l := range links {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM registry_links WHERE urn = ? AND kind = ? AND target = ?`,
				urn, l.Kind, l.Target); err != nil {
				return fmt.Errorf("registry: remove link (%s,%s): %w", l.Kind, l.Target, err)
			}
		}
		return nil
	})
}

// ─── SoftDelete + BumpCachedAt ───────────────────────────────────────────────

// SoftDelete flips status to 'deprecated' on the entry row and bumps
// updated_at. Per D11, the three child tables are intentionally NOT
// touched — cascade-equivalent cleanup is a Go-layer concern for a later
// sprint if/when hard-delete lands.
func (s *Storage) SoftDelete(ctx context.Context, urn string) error {
	if urn == "" {
		return errors.New("registry: soft delete: urn required")
	}
	now := formatTime(time.Now().UTC())
	res, err := s.db.ExecContext(ctx,
		`UPDATE registry_entries SET status = ?, updated_at = ? WHERE urn = ?`,
		string(StatusDeprecated), now, urn)
	if err != nil {
		return fmt.Errorf("registry: soft delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("registry: soft delete %q: %w", urn, ErrNotFound)
	}
	return nil
}

// BumpCachedAt updates cached_at + updated_at on the entry row. Used by
// Sync after a successful callback round-trip.
func (s *Storage) BumpCachedAt(ctx context.Context, urn string, at time.Time) error {
	if urn == "" {
		return errors.New("registry: bump cached_at: urn required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE registry_entries SET cached_at = ?, updated_at = ? WHERE urn = ?`,
		formatTime(at.UTC()), formatTime(time.Now().UTC()), urn)
	if err != nil {
		return fmt.Errorf("registry: bump cached_at: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("registry: bump cached_at %q: %w", urn, ErrNotFound)
	}
	return nil
}

// ─── Search ──────────────────────────────────────────────────────────────────

// Search returns Profiles of the given kind matching the Filter. All
// filter fields combine with AND. Results order alphabetically by
// display_name. Capabilities/skills/links are batch-fetched for the
// matched URNs and assembled in Go.
//
// Status filter:
//   - "" (default) → status = 'active'
//   - "deprecated" → status = 'deprecated'
//   - StatusAny ("*") → no status restriction
func (s *Storage) Search(ctx context.Context, kind Kind, f Filter) ([]Profile, error) {
	// WHERE is built from string literals only; user-supplied filter values
	// flow exclusively into the parameterized args slice.
	where := "kind = ?"
	args := []any{string(kind)}

	switch f.Status {
	case "":
		where += " AND status = ?"
		args = append(args, string(StatusActive))
	case StatusAny:
		// no status restriction
	default:
		where += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Role != "" {
		where += " AND role = ?"
		args = append(args, f.Role)
	}
	if f.Title != "" {
		where += " AND title = ?"
		args = append(args, f.Title)
	}
	if f.Project != "" {
		where += " AND project = ?"
		args = append(args, f.Project)
	}
	if f.Capability != "" {
		where += " AND urn IN (SELECT urn FROM registry_capabilities WHERE capability = ?)"
		args = append(args, f.Capability)
	}
	if f.SkillName != "" {
		where += " AND urn IN (SELECT urn FROM registry_skills WHERE name = ?)"
		args = append(args, f.SkillName)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT urn, kind, mux_instance_id, display_name, title, role, description,
		        avatar, project, status, callback_json, cached_at, health_status,
		        last_seen_at, host_address, merged_into, kind_meta_json, last_updated_by,
		        created_at, updated_at
		   FROM registry_entries
		  WHERE `+where+`
		  ORDER BY display_name ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: search: %w", err)
	}
	defer rows.Close()

	var (
		out   []Profile
		urns  []string
		byURN = map[string]*Profile{}
	)
	for rows.Next() {
		p, err := scanEntryRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
		urns = append(urns, p.URN)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: search rows: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	// Build pointer map after the slice is final so addresses are stable.
	for i := range out {
		byURN[out[i].URN] = &out[i]
	}

	if err := s.batchFillCapabilities(ctx, urns, byURN); err != nil {
		return nil, err
	}
	if err := s.batchFillSkills(ctx, urns, byURN); err != nil {
		return nil, err
	}
	if err := s.batchFillLinks(ctx, urns, byURN); err != nil {
		return nil, err
	}
	if err := s.batchFillExternalIDs(ctx, urns, byURN); err != nil {
		return nil, err
	}
	return out, nil
}

// ─── internal helpers ────────────────────────────────────────────────────────

// replaceChild runs fn inside a transaction and bumps the entry's
// updated_at on the way out. Used by ReplaceCapabilities/Skills/Links.
func (s *Storage) replaceChild(ctx context.Context, urn string, fn func(*sql.Tx) error) error {
	if urn == "" {
		return errors.New("registry: replace: urn required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("registry: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	if err := bumpUpdatedAtTx(ctx, tx, urn); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: commit: %w", err)
	}
	return nil
}

// mutateChild is the append/remove variant — same transaction-with-bump
// shape but distinct from replaceChild for naming clarity.
func (s *Storage) mutateChild(ctx context.Context, urn string, fn func(*sql.Tx) error) error {
	return s.replaceChild(ctx, urn, fn)
}

// bumpUpdatedAtTx sets updated_at = NOW on the entry row within tx. The
// statement requires the row to exist (RowsAffected == 0 means ErrNotFound).
func bumpUpdatedAtTx(ctx context.Context, tx *sql.Tx, urn string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE registry_entries SET updated_at = ? WHERE urn = ?`,
		formatTime(time.Now().UTC()), urn)
	if err != nil {
		return fmt.Errorf("registry: bump updated_at: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("registry: bump updated_at %q: %w", urn, ErrNotFound)
	}
	return nil
}

func (s *Storage) selectEntry(ctx context.Context, urn string) (Profile, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT urn, kind, mux_instance_id, display_name, title, role, description,
		        avatar, project, status, callback_json, cached_at, health_status,
		        last_seen_at, host_address, merged_into, kind_meta_json, last_updated_by,
		        created_at, updated_at
		   FROM registry_entries WHERE urn = ?`, urn)
	p, err := scanEntryRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, fmt.Errorf("registry: get profile %q: %w", urn, ErrNotFound)
	}
	if err != nil {
		return Profile{}, err
	}
	return p, nil
}

func (s *Storage) selectCapabilities(ctx context.Context, urn string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT capability FROM registry_capabilities WHERE urn = ? ORDER BY capability ASC`, urn)
	if err != nil {
		return nil, fmt.Errorf("registry: select capabilities: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("registry: scan capability: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Storage) selectSkills(ctx context.Context, urn string) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, learned_at, via, level
		   FROM registry_skills WHERE urn = ? ORDER BY name ASC`, urn)
	if err != nil {
		return nil, fmt.Errorf("registry: select skills: %w", err)
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		sk, err := scanSkillRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *Storage) selectLinks(ctx context.Context, urn string) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, target FROM registry_links WHERE urn = ? ORDER BY kind ASC, target ASC`, urn)
	if err != nil {
		return nil, fmt.Errorf("registry: select links: %w", err)
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Kind, &l.Target); err != nil {
			return nil, fmt.Errorf("registry: scan link: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Storage) selectExternalIDs(ctx context.Context, urn string) ([]ExternalID, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT substrate, external_id, attached_at
		   FROM registry_external_ids
		  WHERE urn = ?
		  ORDER BY attached_at ASC, substrate ASC`, urn)
	if err != nil {
		return nil, fmt.Errorf("registry: select external ids: %w", err)
	}
	defer rows.Close()
	var out []ExternalID
	for rows.Next() {
		var substrate, externalID, attachedAt string
		if err := rows.Scan(&substrate, &externalID, &attachedAt); err != nil {
			return nil, fmt.Errorf("registry: scan external id: %w", err)
		}
		out = append(out, ExternalID{
			Substrate:  substrate,
			ExternalID: externalID,
			AttachedAt: parseTime(attachedAt),
		})
	}
	return out, rows.Err()
}

func (s *Storage) batchFillCapabilities(ctx context.Context, urns []string, byURN map[string]*Profile) error {
	args := toAnySlice(urns)
	// IN-clause placeholders are derived from len(urns) only — no user text
	// flows into the SQL string; urn values go through parameterized args.
	in := "?"
	in += commaQ(len(urns) - 1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT urn, capability FROM registry_capabilities
		  WHERE urn IN (`+in+`)
		  ORDER BY urn, capability`, args...)
	if err != nil {
		return fmt.Errorf("registry: batch capabilities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var urn, capability string
		if err := rows.Scan(&urn, &capability); err != nil {
			return fmt.Errorf("registry: scan batch capability: %w", err)
		}
		if p, ok := byURN[urn]; ok {
			p.Capabilities = append(p.Capabilities, capability)
		}
	}
	return rows.Err()
}

func (s *Storage) batchFillSkills(ctx context.Context, urns []string, byURN map[string]*Profile) error {
	args := toAnySlice(urns)
	in := "?"
	in += commaQ(len(urns) - 1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT urn, name, learned_at, via, level FROM registry_skills
		  WHERE urn IN (`+in+`)
		  ORDER BY urn, name`, args...)
	if err != nil {
		return fmt.Errorf("registry: batch skills: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			urn, name, learnedAt string
			via, level           sql.NullString
		)
		if err := rows.Scan(&urn, &name, &learnedAt, &via, &level); err != nil {
			return fmt.Errorf("registry: scan batch skill: %w", err)
		}
		sk := Skill{
			Name:      name,
			LearnedAt: parseTime(learnedAt),
			Via:       via.String,
			Level:     level.String,
		}
		if p, ok := byURN[urn]; ok {
			p.Skills = append(p.Skills, sk)
		}
	}
	return rows.Err()
}

func (s *Storage) batchFillLinks(ctx context.Context, urns []string, byURN map[string]*Profile) error {
	args := toAnySlice(urns)
	in := "?"
	in += commaQ(len(urns) - 1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT urn, kind, target FROM registry_links
		  WHERE urn IN (`+in+`)
		  ORDER BY urn, kind, target`, args...)
	if err != nil {
		return fmt.Errorf("registry: batch links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var urn, kind, target string
		if err := rows.Scan(&urn, &kind, &target); err != nil {
			return fmt.Errorf("registry: scan batch link: %w", err)
		}
		if p, ok := byURN[urn]; ok {
			p.Links = append(p.Links, Link{Kind: kind, Target: target})
		}
	}
	return rows.Err()
}

func (s *Storage) batchFillExternalIDs(ctx context.Context, urns []string, byURN map[string]*Profile) error {
	args := toAnySlice(urns)
	in := "?"
	in += commaQ(len(urns) - 1)
	rows, err := s.db.QueryContext(ctx,
		`SELECT urn, substrate, external_id, attached_at FROM registry_external_ids
		  WHERE urn IN (`+in+`)
		  ORDER BY urn, attached_at, substrate`, args...)
	if err != nil {
		return fmt.Errorf("registry: batch external ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var urn, substrate, externalID, attachedAt string
		if err := rows.Scan(&urn, &substrate, &externalID, &attachedAt); err != nil {
			return fmt.Errorf("registry: scan batch external id: %w", err)
		}
		if p, ok := byURN[urn]; ok {
			p.ExternalIDs = append(p.ExternalIDs, ExternalID{
				Substrate:  substrate,
				ExternalID: externalID,
				AttachedAt: parseTime(attachedAt),
			})
		}
	}
	return rows.Err()
}

// scanFn captures the signature shared by *sql.Row.Scan and *sql.Rows.Scan
// so a single scan helper handles both query shapes.
type scanFn func(dest ...any) error

func scanEntryRow(scan scanFn) (Profile, error) {
	var (
		p                                                                                               Profile
		kindStr, status                                                                                 string
		title, role, description, avatar, project, hostAddress, healthStatus, mergedInto, lastUpdatedBy sql.NullString
		callbackJSON, kindMetaJSON                                                                      sql.NullString
		cachedAt, lastSeenAt                                                                            sql.NullString
		createdAt, updatedAt                                                                            string
	)
	if err := scan(
		&p.URN, &kindStr, &p.MuxInstanceID, &p.DisplayName,
		&title, &role, &description, &avatar, &project, &status,
		&callbackJSON, &cachedAt, &healthStatus, &lastSeenAt, &hostAddress, &mergedInto,
		&kindMetaJSON, &lastUpdatedBy, &createdAt, &updatedAt,
	); err != nil {
		return Profile{}, err
	}
	p.Kind = Kind(kindStr)
	p.Status = Status(status)
	p.Title = title.String
	p.Role = role.String
	p.Description = description.String
	p.Avatar = avatar.String
	p.Project = project.String
	p.HealthStatus = healthStatus.String
	p.HostAddress = hostAddress.String
	p.MergedInto = mergedInto.String
	p.LastUpdatedBy = lastUpdatedBy.String

	if callbackJSON.Valid && callbackJSON.String != "" {
		var cb Callback
		if err := json.Unmarshal([]byte(callbackJSON.String), &cb); err != nil {
			return Profile{}, fmt.Errorf("registry: unmarshal callback: %w", err)
		}
		p.Callback = &cb
	}
	if kindMetaJSON.Valid && kindMetaJSON.String != "" {
		p.KindMeta = json.RawMessage(kindMetaJSON.String)
	}
	if cachedAt.Valid && cachedAt.String != "" {
		t := parseTime(cachedAt.String)
		p.CachedAt = &t
	}
	if lastSeenAt.Valid && lastSeenAt.String != "" {
		t := parseTime(lastSeenAt.String)
		p.LastSeenAt = &t
	}
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return p, nil
}

func scanSkillRow(scan scanFn) (Skill, error) {
	var (
		name, learnedAt string
		via, level      sql.NullString
	)
	if err := scan(&name, &learnedAt, &via, &level); err != nil {
		return Skill{}, fmt.Errorf("registry: scan skill: %w", err)
	}
	return Skill{
		Name:      name,
		LearnedAt: parseTime(learnedAt),
		Via:       via.String,
		Level:     level.String,
	}, nil
}

// nullIfEmpty mirrors internal/store/logical_agents.go: an empty string is
// stored as SQL NULL, keeping callers out of sql.NullString.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullIfTimePtr returns NULL for nil/zero, otherwise the formatted UTC time.
func nullIfTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(t.UTC())
}

// formatTime is the canonical time encoding for the registry tables.
// RFC3339Nano matches the convention used by the messaging + events tables.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime parses an RFC3339(Nano) string, tolerating either precision.
// A zero time is returned for unparseable input — callers treat that as
// "not set" rather than propagating a partial error from a scan path.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// toAnySlice converts []string → []any for variadic ExecContext args.
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// commaQ returns n extra ", ?" placeholders for SQL IN clauses. Callers
// supply the leading "?" themselves; this matches the helper shape used by
// internal/store/messaging_store.go (repeatCommaQ).
func commaQ(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(", ?", n)
}

// ─── group ops (v060-05 T-02) ────────────────────────────────────────────────

// InsertGroupWithOwner inserts the group profile + an owner row in
// group_members atomically. Mirrors InsertProfile's body for the registry
// rows, then adds the owner-member insert inside the same transaction.
// Used by Service.Register when kind=group.
func (s *Storage) InsertGroupWithOwner(ctx context.Context, p Profile, ownerURN string) error {
	if p.URN == "" {
		return errors.New("registry: insert group: urn required")
	}
	if ownerURN == "" {
		return errors.New("registry: insert group: owner urn required")
	}
	if p.Kind != KindGroup {
		return fmt.Errorf("registry: insert group: kind must be %q, got %q", KindGroup, p.Kind)
	}

	now := p.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = now
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.MuxInstanceID == "" {
		p.MuxInstanceID = "agent-mux"
	}

	var callbackJSON sql.NullString
	if p.Callback != nil {
		b, err := json.Marshal(p.Callback)
		if err != nil {
			return fmt.Errorf("registry: marshal callback: %w", err)
		}
		callbackJSON = sql.NullString{String: string(b), Valid: true}
	}
	var kindMetaJSON sql.NullString
	if len(p.KindMeta) > 0 {
		kindMetaJSON = sql.NullString{String: string(p.KindMeta), Valid: true}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("registry: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO registry_entries
		    (urn, kind, mux_instance_id, display_name, title, role, description,
		     avatar, project, status, callback_json, cached_at, health_status,
		     last_seen_at, host_address, merged_into, kind_meta_json, last_updated_by,
		     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.URN, string(p.Kind), p.MuxInstanceID, p.DisplayName,
		nullIfEmpty(p.Title), nullIfEmpty(p.Role), nullIfEmpty(p.Description),
		nullIfEmpty(p.Avatar), nullIfEmpty(p.Project), string(p.Status),
		callbackJSON, nullIfTimePtr(p.CachedAt), nullIfEmpty(p.HealthStatus),
		nullIfTimePtr(p.LastSeenAt), nullIfEmpty(p.HostAddress), nullIfEmpty(p.MergedInto),
		kindMetaJSON, nullIfEmpty(p.LastUpdatedBy),
		formatTime(now), formatTime(p.UpdatedAt),
	); err != nil {
		return fmt.Errorf("registry: insert group entry: %w", err)
	}

	for _, c := range p.Capabilities {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_capabilities (urn, capability) VALUES (?, ?)`,
			p.URN, c); err != nil {
			return fmt.Errorf("registry: insert group capability %q: %w", c, err)
		}
	}
	for _, l := range p.Links {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_links (urn, kind, target) VALUES (?, ?, ?)`,
			p.URN, l.Kind, l.Target); err != nil {
			return fmt.Errorf("registry: insert group link (%s,%s): %w", l.Kind, l.Target, err)
		}
	}
	// Skills are technically permitted on group rows by the schema but
	// don't make semantic sense for groups; we still insert any caller-
	// supplied skills for shape consistency with InsertProfile.
	for _, sk := range p.Skills {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO registry_skills (urn, name, learned_at, via, level)
			 VALUES (?, ?, ?, ?, ?)`,
			p.URN, sk.Name, formatTime(sk.LearnedAt),
			nullIfEmpty(sk.Via), nullIfEmpty(sk.Level)); err != nil {
			return fmt.Errorf("registry: insert group skill %q: %w", sk.Name, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO group_members (grp_urn, member_urn, role, joined_at, last_read_seq)
		 VALUES (?, ?, ?, ?, 0)`,
		p.URN, ownerURN, string(MemberRoleOwner), formatTime(now)); err != nil {
		return fmt.Errorf("registry: insert group owner: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: commit group insert: %w", err)
	}
	return nil
}

// InsertGroupMember adds a row to group_members. Used by T-03's AddMember
// after the service-layer role-permission check passes. last_read_seq
// starts at 0 — new members see only messages sent after they joined
// (joined_at gates history; T-04 enforces this).
func (s *Storage) InsertGroupMember(ctx context.Context, grpURN, memberURN string, role MemberRole, joinedAt time.Time) error {
	if grpURN == "" || memberURN == "" {
		return errors.New("registry: insert group member: grpURN + memberURN required")
	}
	if role == "" {
		role = MemberRoleMember
	}
	if joinedAt.IsZero() {
		joinedAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO group_members (grp_urn, member_urn, role, joined_at, last_read_seq)
		 VALUES (?, ?, ?, ?, 0)`,
		grpURN, memberURN, string(role), formatTime(joinedAt)); err != nil {
		return fmt.Errorf("registry: insert group member: %w", err)
	}
	return nil
}

// GroupMemberRole returns the role of memberURN within grpURN. If
// memberURN is not a member, returns ("", false, nil). Storage errors
// surface as ("", false, err).
func (s *Storage) GroupMemberRole(ctx context.Context, grpURN, memberURN string) (MemberRole, bool, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM group_members WHERE grp_urn = ? AND member_urn = ?`,
		grpURN, memberURN).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("registry: group member role: %w", err)
	}
	return MemberRole(role), true, nil
}

// ListGroupsForMember returns the group profiles memberURN belongs to,
// ordered alphabetically by display_name. Mirrors Search's
// children-hydration pattern.
func (s *Storage) ListGroupsForMember(ctx context.Context, memberURN string) ([]Profile, error) {
	if memberURN == "" {
		return nil, errors.New("registry: list groups for member: memberURN required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.urn, e.kind, e.mux_instance_id, e.display_name, e.title, e.role,
		        e.description, e.avatar, e.project, e.status, e.callback_json,
		        e.cached_at, e.health_status, e.last_seen_at, e.host_address, e.merged_into,
		        e.kind_meta_json, e.last_updated_by, e.created_at, e.updated_at
		   FROM registry_entries e
		   JOIN group_members m ON m.grp_urn = e.urn
		  WHERE m.member_urn = ?
		  ORDER BY e.display_name ASC`, memberURN)
	if err != nil {
		return nil, fmt.Errorf("registry: list groups for member: %w", err)
	}
	defer rows.Close()

	var (
		out   []Profile
		urns  []string
		byURN = map[string]*Profile{}
	)
	for rows.Next() {
		p, err := scanEntryRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
		urns = append(urns, p.URN)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list groups for member rows: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	for i := range out {
		byURN[out[i].URN] = &out[i]
	}
	if err := s.batchFillCapabilities(ctx, urns, byURN); err != nil {
		return nil, err
	}
	if err := s.batchFillSkills(ctx, urns, byURN); err != nil {
		return nil, err
	}
	if err := s.batchFillLinks(ctx, urns, byURN); err != nil {
		return nil, err
	}
	return out, nil
}

// ListMembers returns the group_members rows for grpURN with display_name
// hydrated from registry_entries via JOIN. Ordered by joined_at ASC so
// older members surface first. Used by T-03's Service.ListMembers.
func (s *Storage) ListMembers(ctx context.Context, grpURN string) ([]GroupMember, error) {
	if grpURN == "" {
		return nil, errors.New("registry: list members: grpURN required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.grp_urn, m.member_urn, m.role, m.joined_at, m.last_read_seq,
		        e.display_name
		   FROM group_members m
		   LEFT JOIN registry_entries e ON e.urn = m.member_urn
		  WHERE m.grp_urn = ?
		  ORDER BY m.joined_at ASC`, grpURN)
	if err != nil {
		return nil, fmt.Errorf("registry: list members: %w", err)
	}
	defer rows.Close()
	var out []GroupMember
	for rows.Next() {
		var (
			gm          GroupMember
			role        string
			joinedAt    string
			displayName sql.NullString
		)
		if err := rows.Scan(&gm.GroupURN, &gm.MemberURN, &role, &joinedAt, &gm.LastReadSeq, &displayName); err != nil {
			return nil, fmt.Errorf("registry: list members scan: %w", err)
		}
		gm.Role = MemberRole(role)
		gm.JoinedAt = parseTime(joinedAt)
		if displayName.Valid {
			gm.DisplayName = displayName.String
		}
		out = append(out, gm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list members rows: %w", err)
	}
	return out, nil
}

// RemoveGroupMember deletes a row from group_members. Returns ErrNotFound
// if no row was deleted. Used by T-03's RemoveMember / LeaveGroup.
func (s *Storage) RemoveGroupMember(ctx context.Context, grpURN, memberURN string) error {
	if grpURN == "" || memberURN == "" {
		return errors.New("registry: remove group member: grpURN + memberURN required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM group_members WHERE grp_urn = ? AND member_urn = ?`,
		grpURN, memberURN)
	if err != nil {
		return fmt.Errorf("registry: remove group member: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registry: remove group member rows-affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("registry: remove group member: %w: (%q,%q)", ErrNotFound, grpURN, memberURN)
	}
	return nil
}

// UpdateGroupMemberRole sets the role of memberURN in grpURN. Returns
// ErrNotFound if no row matched. Used by T-03's SetMemberRole.
func (s *Storage) UpdateGroupMemberRole(ctx context.Context, grpURN, memberURN string, role MemberRole) error {
	if grpURN == "" || memberURN == "" {
		return errors.New("registry: update group member role: grpURN + memberURN required")
	}
	if role == "" {
		return errors.New("registry: update group member role: role required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE group_members SET role = ? WHERE grp_urn = ? AND member_urn = ?`,
		string(role), grpURN, memberURN)
	if err != nil {
		return fmt.Errorf("registry: update group member role: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registry: update group member role rows-affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("registry: update group member role: %w: (%q,%q)", ErrNotFound, grpURN, memberURN)
	}
	return nil
}

// FindByDisplayName returns all profiles with display_name=name,
// excluding deprecated/archived rows by default. Mention resolution
// uses this to detect ambiguous @-tokens: len(out)>1 → ambiguous.
func (s *Storage) FindByDisplayName(ctx context.Context, name string) ([]Profile, error) {
	if name == "" {
		return nil, errors.New("registry: find by display name: name required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT urn, kind, mux_instance_id, display_name, title, role, description,
		        avatar, project, status, callback_json, cached_at, health_status,
		        last_seen_at, host_address, merged_into, kind_meta_json, last_updated_by,
		        created_at, updated_at
		   FROM registry_entries
		  WHERE display_name = ?
		    AND status = ?
		  ORDER BY urn ASC`, name, string(StatusActive))
	if err != nil {
		return nil, fmt.Errorf("registry: find by display name: %w", err)
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := scanEntryRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: find by display name rows: %w", err)
	}
	return out, nil
}

// InsertGroupMessage writes a row to the messages table with group_urn +
// the next group_seq for this group, inside a single transaction. The
// MaxOpenConns=1 invariant in internal/store/sqlite.go serializes all
// writes, so MAX+1 within a tx is race-safe without IMMEDIATE locks.
// Returns the inserted GroupMessage with ID + GroupSeq + CreatedAt filled.
//
// to_urn is set to grpURN for column-NOT-NULL compliance with the
// shared messages table; the existing /messages/* HTTP path rejects
// group URNs at the go-messaging.ParseURN boundary (closed AddressKind
// enum), so personal-inbox queries cannot accidentally surface group
// rows. Group reads go through ListGroupMessages (which queries by
// group_urn, not to_urn).
func (s *Storage) InsertGroupMessage(ctx context.Context, grpURN, fromURN, kind, threadID, contentType string, payload json.RawMessage) (GroupMessage, error) {
	if grpURN == "" || fromURN == "" || kind == "" {
		return GroupMessage{}, errors.New("registry: insert group message: grpURN + fromURN + kind required")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return GroupMessage{}, fmt.Errorf("registry: insert group message: uuid: %w", err)
	}
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return GroupMessage{}, fmt.Errorf("registry: begin group-message tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(group_seq) FROM messages WHERE group_urn = ?`, grpURN).Scan(&maxSeq); err != nil {
		return GroupMessage{}, fmt.Errorf("registry: insert group message: seq max: %w", err)
	}
	nextSeq := int64(1)
	if maxSeq.Valid {
		nextSeq = maxSeq.Int64 + 1
	}

	payloadStr := ""
	if len(payload) > 0 {
		payloadStr = string(payload)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages
		 (id, kind, from_urn, to_urn, thread_id, payload, content_type,
		  created_at, group_urn, group_seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), kind, fromURN, grpURN,
		nullIfEmpty(threadID), nullIfEmpty(payloadStr), nullIfEmpty(contentType),
		now.Format(time.RFC3339Nano), grpURN, nextSeq); err != nil {
		return GroupMessage{}, fmt.Errorf("registry: insert group message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return GroupMessage{}, fmt.Errorf("registry: commit group message: %w", err)
	}
	return GroupMessage{
		ID:          id.String(),
		GroupURN:    grpURN,
		GroupSeq:    nextSeq,
		FromURN:     fromURN,
		Kind:        kind,
		ThreadID:    threadID,
		Payload:     payload,
		ContentType: contentType,
		CreatedAt:   now,
	}, nil
}

// ListGroupMessages returns messages addressed to grpURN with group_seq
// > sinceSeq, optionally filtered by threadID, with a hard limit. Rows
// created before joinedAt are excluded (new members can't read history
// that predates their membership; D5 semantics). Ordered by group_seq
// ASC for deterministic pagination.
func (s *Storage) ListGroupMessages(ctx context.Context, grpURN string, sinceSeq int64, threadID string, limit int, joinedAt time.Time) ([]GroupMessage, error) {
	if grpURN == "" {
		return nil, errors.New("registry: list group messages: grpURN required")
	}
	if limit <= 0 {
		limit = 100
	}
	where := "group_urn = ? AND group_seq > ?"
	args := []any{grpURN, sinceSeq}
	if !joinedAt.IsZero() {
		where += " AND created_at >= ?"
		args = append(args, joinedAt.Format(time.RFC3339Nano))
	}
	if threadID != "" {
		where += " AND thread_id = ?"
		args = append(args, threadID)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, from_urn, thread_id, payload, content_type,
		        created_at, group_seq
		   FROM messages
		  WHERE `+where+`
		  ORDER BY group_seq ASC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: list group messages: %w", err)
	}
	defer rows.Close()
	var out []GroupMessage
	for rows.Next() {
		var (
			gm           GroupMessage
			threadIDN    sql.NullString
			payload      sql.NullString
			contentTypeN sql.NullString
			createdAt    string
		)
		if err := rows.Scan(&gm.ID, &gm.Kind, &gm.FromURN, &threadIDN, &payload,
			&contentTypeN, &createdAt, &gm.GroupSeq); err != nil {
			return nil, fmt.Errorf("registry: list group messages scan: %w", err)
		}
		gm.GroupURN = grpURN
		if threadIDN.Valid {
			gm.ThreadID = threadIDN.String
		}
		if payload.Valid {
			gm.Payload = json.RawMessage(payload.String)
		}
		if contentTypeN.Valid {
			gm.ContentType = contentTypeN.String
		}
		gm.CreatedAt = parseTime(createdAt)
		out = append(out, gm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list group messages rows: %w", err)
	}
	return out, nil
}

// BumpGroupReadCursor sets last_read_seq = max(last_read_seq, upToSeq)
// for (grpURN, memberURN). Monotonic by construction; smaller upToSeq
// values are no-ops. Returns ErrNotFound if no membership row matched.
func (s *Storage) BumpGroupReadCursor(ctx context.Context, grpURN, memberURN string, upToSeq int64) error {
	if grpURN == "" || memberURN == "" {
		return errors.New("registry: bump read cursor: grpURN + memberURN required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE group_members
		    SET last_read_seq = max(last_read_seq, ?)
		  WHERE grp_urn = ? AND member_urn = ?`,
		upToSeq, grpURN, memberURN)
	if err != nil {
		return fmt.Errorf("registry: bump read cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registry: bump read cursor rows-affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("registry: bump read cursor: %w: (%q,%q)", ErrNotFound, grpURN, memberURN)
	}
	return nil
}

// GroupMemberJoinedAt returns the joined_at timestamp for memberURN in
// grpURN. Used by ListGroupMessages to gate pre-join history. Returns
// (zero time, false, nil) when not a member.
func (s *Storage) GroupMemberJoinedAt(ctx context.Context, grpURN, memberURN string) (time.Time, bool, error) {
	var joinedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT joined_at FROM group_members WHERE grp_urn = ? AND member_urn = ?`,
		grpURN, memberURN).Scan(&joinedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("registry: joined-at: %w", err)
	}
	return parseTime(joinedAt), true, nil
}

// GroupMemberLastReadSeq returns the read cursor for memberURN in grpURN.
// Returns (0, false, nil) when not a member.
func (s *Storage) GroupMemberLastReadSeq(ctx context.Context, grpURN, memberURN string) (int64, bool, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT last_read_seq FROM group_members WHERE grp_urn = ? AND member_urn = ?`,
		grpURN, memberURN).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("registry: last-read-seq: %w", err)
	}
	return seq, true, nil
}

// ListMentionsForMember returns notice envelopes addressed to memberURN
// whose payload carries a `group` key (set by T-05's mention parser).
// Optional sinceTS filters to created_at >= sinceTS. Ordered created_at
// DESC (newest first) for activity-feed display.
func (s *Storage) ListMentionsForMember(ctx context.Context, memberURN string, sinceTS time.Time, limit int) ([]GroupMessage, error) {
	if memberURN == "" {
		return nil, errors.New("registry: list mentions: memberURN required")
	}
	if limit <= 0 {
		limit = 50
	}
	where := "to_urn = ? AND kind = 'notice' AND json_extract(payload, '$.group') IS NOT NULL"
	args := []any{memberURN}
	if !sinceTS.IsZero() {
		where += " AND created_at >= ?"
		args = append(args, sinceTS.Format(time.RFC3339Nano))
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, from_urn, thread_id, payload, content_type,
		        created_at, group_urn
		   FROM messages
		  WHERE `+where+`
		  ORDER BY created_at DESC
		  LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: list mentions: %w", err)
	}
	defer rows.Close()
	var out []GroupMessage
	for rows.Next() {
		var (
			gm           GroupMessage
			threadIDN    sql.NullString
			payload      sql.NullString
			contentTypeN sql.NullString
			createdAt    string
			groupURN     sql.NullString
		)
		if err := rows.Scan(&gm.ID, &gm.Kind, &gm.FromURN, &threadIDN, &payload,
			&contentTypeN, &createdAt, &groupURN); err != nil {
			return nil, fmt.Errorf("registry: list mentions scan: %w", err)
		}
		if threadIDN.Valid {
			gm.ThreadID = threadIDN.String
		}
		if payload.Valid {
			gm.Payload = json.RawMessage(payload.String)
		}
		if contentTypeN.Valid {
			gm.ContentType = contentTypeN.String
		}
		gm.CreatedAt = parseTime(createdAt)
		if groupURN.Valid {
			gm.GroupURN = groupURN.String
		}
		out = append(out, gm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list mentions rows: %w", err)
	}
	return out, nil
}

// CountModeratorsExcluding counts members of grpURN with role='moderator'
// or role='owner', excluding excludeURN. Used by T-03's LeaveGroup to
// guard the "owner cannot leave without a moderator successor" semantic.
// Counting owner-role rows defensively in case multiple owners ever exist.
func (s *Storage) CountModeratorsExcluding(ctx context.Context, grpURN, excludeURN string) (int, error) {
	if grpURN == "" {
		return 0, errors.New("registry: count moderators: grpURN required")
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM group_members
		  WHERE grp_urn = ? AND member_urn <> ?
		    AND role IN ('moderator','owner')`,
		grpURN, excludeURN).Scan(&n); err != nil {
		return 0, fmt.Errorf("registry: count moderators: %w", err)
	}
	return n, nil
}

// SetProfileStatus updates only the status column + updated_at. Used by
// ArchiveGroup (sets status='archived' per D9). Returns ErrNotFound if
// no row matched.
func (s *Storage) SetProfileStatus(ctx context.Context, urn string, status Status) error {
	if urn == "" {
		return errors.New("registry: set status: urn required")
	}
	if status == "" {
		return errors.New("registry: set status: status required")
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE registry_entries SET status = ?, updated_at = ? WHERE urn = ?`,
		string(status), formatTime(now), urn)
	if err != nil {
		return fmt.Errorf("registry: set status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registry: set status rows-affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("registry: set status: %w: urn=%q", ErrNotFound, urn)
	}
	return nil
}
