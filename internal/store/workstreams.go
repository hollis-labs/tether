package store

// workstreams.go — the durable container a unit of work lives in, and the
// lineage inheritance that keeps it attached across a compaction.
//
// S1 of SP-20260912-0001 (CW-20260912-0059); design record CW-20260912-0023.
//
// The whole point is stated in one place, migration 0023: a compaction creates
// a NEW session row, so anything keyed on session_id is orphaned by the very
// event the container exists to survive. Inheritance therefore has to be
// structural rather than remembered — see inheritWorkstreamID, which runs
// inside CreateSession so no caller can skip it.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrWorkstreamNotFound is returned by GetWorkstream and by the assign path
// when the named workstream does not exist.
var ErrWorkstreamNotFound = errors.New("workstream not found")

// WorkstreamRow mirrors the workstreams table.
type WorkstreamRow struct {
	ID   string
	Name string
	// WorkflowID correlates this workstream with a workflow owned by some
	// other system. Free-form and unresolved here by design (migration 0023).
	WorkflowID string
	// Status is "active" or "closed"; empty defaults to "active" on write.
	Status    string
	CreatedAt string
	UpdatedAt string
}

// CreateWorkstream inserts a workstream, generating an id when one is not
// supplied. The generated id is a UUIDv7 so the primary key sorts by creation
// time, matching what messaging_store.go does for envelopes.
func (s *Store) CreateWorkstream(w WorkstreamRow) (WorkstreamRow, error) {
	if w.ID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return WorkstreamRow{}, fmt.Errorf("create workstream: uuid v7: %w", err)
		}
		w.ID = id.String()
	}
	if w.Status == "" {
		w.Status = "active"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	w.CreatedAt, w.UpdatedAt = now, now
	if _, err := s.db.Exec(
		`INSERT INTO workstreams (id, name, workflow_id, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		w.ID, nullIfEmpty(w.Name), nullIfEmpty(w.WorkflowID), w.Status, w.CreatedAt, w.UpdatedAt,
	); err != nil {
		return WorkstreamRow{}, fmt.Errorf("create workstream %q: %w", w.ID, err)
	}
	return w, nil
}

// GetWorkstream fetches one workstream by id, returning ErrWorkstreamNotFound
// when it does not exist.
func (s *Store) GetWorkstream(id string) (*WorkstreamRow, error) {
	var w WorkstreamRow
	var name, workflowID sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, workflow_id, status, created_at, updated_at FROM workstreams WHERE id=?`, id,
	).Scan(&w.ID, &name, &workflowID, &w.Status, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkstreamNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get workstream %q: %w", id, err)
	}
	w.Name, w.WorkflowID = name.String, workflowID.String
	return &w, nil
}

// ListWorkstreamsOptions filters ListWorkstreams. Zero value lists everything,
// newest first.
type ListWorkstreamsOptions struct {
	Status     string
	WorkflowID string
	Limit      int
}

// ListWorkstreams returns workstreams newest-first.
func (s *Store) ListWorkstreams(opts ListWorkstreamsOptions) ([]WorkstreamRow, error) {
	where := ""
	args := []any{}
	if opts.Status != "" {
		where += " WHERE status = ?"
		args = append(args, opts.Status)
	}
	if opts.WorkflowID != "" {
		if where == "" {
			where = " WHERE workflow_id = ?"
		} else {
			where += " AND workflow_id = ?"
		}
		args = append(args, opts.WorkflowID)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit)

	rows, err := s.db.Query(
		`SELECT id, name, workflow_id, status, created_at, updated_at FROM workstreams`+
			where+` ORDER BY created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list workstreams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkstreamRow
	for rows.Next() {
		var w WorkstreamRow
		var name, workflowID sql.NullString
		if err := rows.Scan(&w.ID, &name, &workflowID, &w.Status, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list workstreams: scan: %w", err)
		}
		w.Name, w.WorkflowID = name.String, workflowID.String
		out = append(out, w)
	}
	return out, rows.Err()
}

// AssignSessionWorkstream stamps workstreamID onto sessionID. Passing an empty
// workstreamID clears the association.
//
// The workstream is verified to exist first: a dangling workstream_id would be
// indistinguishable from an unassigned session at read time, since ADR-0008
// leaves FK enforcement off.
func (s *Store) AssignSessionWorkstream(sessionID, workstreamID string) error {
	if sessionID == "" {
		return errors.New("assign workstream: session id required")
	}
	if workstreamID != "" {
		if _, err := s.GetWorkstream(workstreamID); err != nil {
			return err
		}
	}
	res, err := s.db.Exec(
		`UPDATE sessions SET workstream_id=?, updated_at=? WHERE id=?`,
		nullIfEmpty(workstreamID), time.Now().UTC().Format(time.RFC3339), sessionID,
	)
	if err != nil {
		return fmt.Errorf("assign workstream to session %q: %w", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("assign workstream to session %q: %w", sessionID, err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// EnsureSessionWorkstream returns the workstream containing sessionID,
// creating one if the lineage has none yet. It is the "a workstream is always
// available without being ceremony" half of S1.
//
// Resolution order, and each step is load-bearing:
//
//  1. The session's own workstream_id, if set. The common case after
//     inheritance has done its job.
//  2. An ancestor's, walking parent_session_id to the lineage root. This is
//     what makes a SIBLING converge: a fork created before the workstream
//     existed inherited nothing at creation, and finds it here instead of
//     minting a second container for the same work.
//  3. A fresh workstream, stamped onto every session from the lineage root
//     down to this one — not just this one. Stamping only the caller's session
//     would leave its own parent outside the container, which is the
//     orphaning this task exists to prevent, merely pointed the other way.
func (s *Store) EnsureSessionWorkstream(sessionID string, seed WorkstreamRow) (WorkstreamRow, error) {
	chain, err := s.sessionLineage(sessionID)
	if err != nil {
		return WorkstreamRow{}, err
	}

	// Steps 1 and 2: nearest existing workstream, self first then ancestors.
	for _, row := range chain {
		if row.WorkstreamID.Valid && row.WorkstreamID.String != "" {
			existing, err := s.GetWorkstream(row.WorkstreamID.String)
			if err != nil {
				// A dangling pointer is corruption, not an empty lineage;
				// surface it rather than silently minting a second
				// container beside the one that was meant to be there.
				return WorkstreamRow{}, fmt.Errorf("session %q references workstream %q: %w", row.ID, row.WorkstreamID.String, err)
			}
			// Backfill any descendant that was missing it, so the whole
			// chain converges on one id rather than re-resolving each call.
			if err := s.stampLineage(chain, existing.ID); err != nil {
				return WorkstreamRow{}, err
			}
			return *existing, nil
		}
	}

	// Step 3: nothing in the lineage has one.
	created, err := s.CreateWorkstream(seed)
	if err != nil {
		return WorkstreamRow{}, err
	}
	if err := s.stampLineage(chain, created.ID); err != nil {
		return WorkstreamRow{}, err
	}
	return created, nil
}

// stampLineage fills GAPS: it assigns workstreamID to sessions in chain that
// carry no workstream, and leaves any session already carrying a DIFFERENT one
// untouched.
//
// Divergence within one lineage is reachable — an operator can assign a
// workstream directly to a mid-lineage session — so the behavior has to be
// defined rather than left to whatever the walk happens to do. It is defined
// as nearest-ancestor-wins for the ANSWER (EnsureSessionWorkstream returns the
// closest workstream it finds) and never-overwrite for the WRITE.
//
// Never-overwrite is the half that matters. An earlier version of this
// function skipped only rows already carrying the SAME id, so a further
// ancestor holding a different workstream was silently re-parented into the
// nearer one. Quietly moving work out of a container an operator deliberately
// put it in is worse than leaving a lineage that spans two containers, and it
// is unrecoverable — the previous association is gone with nothing recording
// that it existed.
func (s *Store) stampLineage(chain []SessionRow, workstreamID string) error {
	for _, row := range chain {
		if row.WorkstreamID.Valid && row.WorkstreamID.String != "" {
			// Already placed, here or elsewhere. Either way, not ours to move.
			continue
		}
		if err := s.AssignSessionWorkstream(row.ID, workstreamID); err != nil {
			return err
		}
	}
	return nil
}

// sessionLineage returns sessionID and its ancestors, nearest first, walking
// parent_session_id to the root.
//
// The visited set is not paranoia about a malformed DB: parent_session_id has
// no FK enforcement (ADR-0008) and nothing rejects a cycle on write, so a bad
// row would otherwise spin here forever rather than fail.
func (s *Store) sessionLineage(sessionID string) ([]SessionRow, error) {
	var chain []SessionRow
	visited := map[string]bool{}
	for id := sessionID; id != ""; {
		if visited[id] {
			return nil, fmt.Errorf("session lineage for %q: cycle at %q", sessionID, id)
		}
		visited[id] = true

		row, err := s.GetSession(id)
		if err != nil {
			// A parent row that no longer exists ends the walk rather than
			// failing it — ON DELETE SET NULL is declarative only here, so a
			// deleted ancestor legitimately leaves a stale pointer behind.
			if errors.Is(err, ErrSessionNotFound) && id != sessionID {
				break
			}
			return nil, err
		}
		chain = append(chain, *row)
		if !row.ParentSessionID.Valid {
			break
		}
		id = row.ParentSessionID.String
	}
	return chain, nil
}

// inheritWorkstreamID resolves the workstream a newly-created session should
// carry. It is called from CreateSession, which is the ONLY path any session
// row reaches the DB through — that is what makes inheritance structural
// rather than something each of the three creation call sites (launch, resume,
// /sessions/bootstrap) has to remember.
//
// An explicit workstream on the incoming row always wins; this only fills a
// gap.
//
// THE SIGNAL IS parent_session_id, NOT intent. Those two fields answer
// different questions: intent says WHY a session exists, parent_session_id
// says WHAT it continues. They are orthogonal, so gating an inheritance
// decision on intent is what lets them disagree — a caller can set
// intent='preassigned' (which describes only how the id was chosen) together
// with a parent, and the enumeration would then refuse to honor a lineage the
// caller explicitly asserted.
//
// An enumeration also rots: a sixth intent arrives, someone has to remember to
// classify it, and the failure is silent and looks exactly like the orphaning
// this whole feature exists to prevent. Presence of a parent cannot rot —
// there is no other reading of that field than "this continues that".
//
// A parented session that should get its OWN container is still expressible:
// assign one explicitly afterward. The common case is automatic and the
// exception is deliberate, which is the right way round.
func (s *Store) inheritWorkstreamID(row SessionRow) sql.NullString {
	if row.WorkstreamID.Valid && row.WorkstreamID.String != "" {
		return row.WorkstreamID
	}
	if !row.ParentSessionID.Valid || row.ParentSessionID.String == "" {
		return sql.NullString{}
	}
	parent, err := s.GetSession(row.ParentSessionID.String)
	if err != nil {
		// Inheritance is best-effort: a missing parent must not fail the
		// launch. The session is still created, just without a container,
		// and EnsureSessionWorkstream can give it one later.
		return sql.NullString{}
	}
	return parent.WorkstreamID
}
