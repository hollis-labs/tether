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
