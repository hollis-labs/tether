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
		     last_seen_at, host_address, kind_meta_json, last_updated_by,
		     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.URN, string(p.Kind), p.MuxInstanceID, p.DisplayName,
		nullIfEmpty(p.Title), nullIfEmpty(p.Role), nullIfEmpty(p.Description),
		nullIfEmpty(p.Avatar), nullIfEmpty(p.Project), string(p.Status),
		callbackJSON, nullIfTimePtr(p.CachedAt), nullIfEmpty(p.HealthStatus),
		nullIfTimePtr(p.LastSeenAt), nullIfEmpty(p.HostAddress),
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
	p.Capabilities = caps
	p.Skills = skills
	p.Links = links
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
		        last_seen_at, host_address, kind_meta_json, last_updated_by,
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
	p.Capabilities = caps
	p.Skills = skills
	p.Links = links
	return p, nil
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
		        last_seen_at, host_address, kind_meta_json, last_updated_by,
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
		        last_seen_at, host_address, kind_meta_json, last_updated_by,
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

// scanFn captures the signature shared by *sql.Row.Scan and *sql.Rows.Scan
// so a single scan helper handles both query shapes.
type scanFn func(dest ...any) error

func scanEntryRow(scan scanFn) (Profile, error) {
	var (
		p                                                                                   Profile
		kindStr, status                                                                     string
		title, role, description, avatar, project, hostAddress, healthStatus, lastUpdatedBy sql.NullString
		callbackJSON, kindMetaJSON                                                          sql.NullString
		cachedAt, lastSeenAt                                                                sql.NullString
		createdAt, updatedAt                                                                string
	)
	if err := scan(
		&p.URN, &kindStr, &p.MuxInstanceID, &p.DisplayName,
		&title, &role, &description, &avatar, &project, &status,
		&callbackJSON, &cachedAt, &healthStatus, &lastSeenAt, &hostAddress,
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
		     last_seen_at, host_address, kind_meta_json, last_updated_by,
		     created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.URN, string(p.Kind), p.MuxInstanceID, p.DisplayName,
		nullIfEmpty(p.Title), nullIfEmpty(p.Role), nullIfEmpty(p.Description),
		nullIfEmpty(p.Avatar), nullIfEmpty(p.Project), string(p.Status),
		callbackJSON, nullIfTimePtr(p.CachedAt), nullIfEmpty(p.HealthStatus),
		nullIfTimePtr(p.LastSeenAt), nullIfEmpty(p.HostAddress),
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
		        e.cached_at, e.health_status, e.last_seen_at, e.host_address,
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

// SetProfileStatus updates only the status column + updated_at. Used by
// ArchiveGroup (sets status='deprecated' or status='archived'). v1 group
// archive uses StatusDeprecated to share the soft-delete pattern from
// D11; consumers detecting "archived" should check status='deprecated'.
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
