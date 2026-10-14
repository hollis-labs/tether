package store

import "fmt"

// ChildSessionIDs returns the ids of sessions whose parent_session_id is id,
// oldest first.
func (s *Store) ChildSessionIDs(id string) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE parent_session_id=? ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SessionLineageDepth counts the ancestors of id by walking parent_session_id
// (a root session has depth 0). The walk is bounded so a corrupt cycle cannot
// loop forever.
func (s *Store) SessionLineageDepth(id string) (int, error) {
	const maxWalk = 1024
	depth := 0
	for cur := id; depth < maxWalk; depth++ {
		var parent *string
		if err := s.db.QueryRow(`SELECT parent_session_id FROM sessions WHERE id=?`, cur).Scan(&parent); err != nil {
			return 0, fmt.Errorf("lineage depth %s: %w", cur, err)
		}
		if parent == nil || *parent == "" {
			return depth, nil
		}
		cur = *parent
	}
	return depth, nil
}

// LiveChildCount counts the direct children of id that have not ended
// (created, ready, launching, running, detached or orphaned). Ended children
// do not count against a parent's child cap.
func (s *Store) LiveChildCount(id string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE parent_session_id=? AND state IN ('created','ready','launching','running','detached','orphaned')`, id).Scan(&n)
	return n, err
}

// ChildCountSince counts every direct child of id (any state) created at or
// after cutoff, an RFC3339 UTC timestamp.
func (s *Store) ChildCountSince(id, cutoff string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE parent_session_id=? AND created_at>=?`, id, cutoff).Scan(&n)
	return n, err
}
