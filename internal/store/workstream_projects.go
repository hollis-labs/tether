package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
)

var ErrWorkstreamProject = errors.New("invalid workstream project association")

// ValidProjectURN checks canonical registry project identity, not launch IDs.
// Existence and registry kind/status are checked transactionally on writes.
func ValidProjectURN(urn string) bool {
	a, err := messaging.ParseURN(urn)
	return err == nil && a.Kind == messaging.KindAgent && strings.HasPrefix(a.ID, "prj_") && len(a.ID) > 4 && a.URN() == urn
}

func replaceWorkstreamProjects(tx *sql.Tx, id string, requested []string) ([]string, error) {
	// An explicit replacement, including [], requires the registry to be
	// available. Unassociated legacy creates do not require registry admission.
	if requested != nil {
		var probe int
		if err := tx.QueryRow(`SELECT 1 FROM registry_entries LIMIT 1`).Scan(&probe); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("registry project lookup unavailable: %w", err)
		}
	}

	projects := append([]string{}, requested...)
	slices.Sort(projects)
	projects = slices.Compact(projects)
	for _, urn := range projects {
		if !ValidProjectURN(urn) {
			return nil, ErrWorkstreamProject
		}
		var kind, status string
		err := tx.QueryRow(`SELECT kind,status FROM registry_entries WHERE urn=?`, urn).Scan(&kind, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrWorkstreamProject
		}
		if err != nil {
			return nil, fmt.Errorf("registry project lookup unavailable: %w", err)
		}
		if kind != "project" {
			return nil, ErrWorkstreamProject
		}
		if status == "deprecated" {
			var linked bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workstream_projects WHERE workstream_id=? AND project_urn=?)`, id, urn).Scan(&linked); err != nil {
				return nil, err
			}
			if !linked {
				return nil, ErrWorkstreamProject
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM workstream_projects WHERE workstream_id=?`, id); err != nil {
		return nil, err
	}
	for _, urn := range projects {
		if _, err := tx.Exec(`INSERT INTO workstream_projects(workstream_id,project_urn) VALUES (?,?)`, id, urn); err != nil {
			return nil, err
		}
	}
	return projects, nil
}

// loadWorkstreamProjects uses one batch read for the returned workstreams.
// Membership remains visible after a registry project is deprecated.
func (s *Store) loadWorkstreamProjects(items []WorkstreamRow) ([]WorkstreamRow, error) {
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
		items[i].ProjectURNs = []string{}
	}
	if len(ids) == 0 {
		return items, nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT workstream_id,project_urn FROM workstream_projects WHERE workstream_id IN (SELECT value FROM json_each(?)) ORDER BY workstream_id,project_urn`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string][]string{}
	for rows.Next() {
		var id, urn string
		if err := rows.Scan(&id, &urn); err != nil {
			return nil, err
		}
		byID[id] = append(byID[id], urn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range items {
		if projects, ok := byID[items[i].ID]; ok {
			items[i].ProjectURNs = projects
		}
	}
	return items, nil
}
