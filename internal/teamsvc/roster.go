package teamsvc

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

// RosterRequest selects retained membership, never caller identity or authority.
type RosterRequest struct {
	RunID string
	After string
	Limit int
}

// RosterView exposes public run/member metadata without host receipts or secrets.
type RosterView struct {
	Run     teams.TeamRun `json:"run"`
	Version uint64        `json:"roster_version"`
	Members []MemberView  `json:"members"`
}

type RosterList struct {
	Runs      []RosterView `json:"runs"`
	NextAfter string       `json:"next_after,omitempty"`
}

type rosterLister interface {
	ListRostersForActor(context.Context, mesh.URN, mesh.ActorKind, string, int) ([]teams.Roster, error)
}

// ValidateRosterRequest applies identical bounded selectors on every surface.
func ValidateRosterRequest(req RosterRequest) error {
	if req.Limit < 0 || req.Limit > 100 || req.RunID != "" && req.After != "" {
		return ErrInvalidRequest
	}
	for _, value := range []string{req.RunID, req.After} {
		if len(value) > MaxKeyBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsFunc(value, unicode.IsControl) {
			return ErrInvalidRequest
		}
	}
	return nil
}

// ListRoster authenticates before any storage read. Listing never journals a
// mutation, grants operator privileges, or reveals runs to nonmembers. Retained
// members may inspect their ended run so stop/recovery outcomes remain visible.
func (s *Service) ListRoster(ctx context.Context, req RosterRequest) (RosterList, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return RosterList{}, typed(err)
	}
	if err = ValidateRosterRequest(req); err != nil {
		return RosterList{}, typed(err)
	}
	limit := req.Limit
	if limit == 0 {
		limit = 50
	}
	var rosters []teams.Roster
	if req.RunID != "" {
		roster, snapshotErr := s.deps.Roster.Snapshot(ctx, req.RunID)
		if snapshotErr != nil {
			return RosterList{}, typed(snapshotErr)
		}
		rosters = []teams.Roster{roster}
	} else {
		reader, ok := s.deps.Roster.(rosterLister)
		if !ok {
			return RosterList{}, typed(ErrUnavailable)
		}
		rosters, err = reader.ListRostersForActor(ctx, p.ID, p.Kind, req.After, limit+1)
		if err != nil {
			return RosterList{}, typed(err)
		}
	}
	result := RosterList{Runs: []RosterView{}}
	if len(rosters) > limit {
		rosters = rosters[:limit]
		result.NextAfter = rosters[len(rosters)-1].RunID
	}
	for _, roster := range rosters {
		member := false
		for _, m := range roster.Members {
			if m.Actor == p.ID && m.Kind == p.Kind && retainedRosterStatus(m.Status) {
				member = true
			}
		}
		if !member {
			return RosterList{}, typed(ErrNotFound)
		}
		if req.RunID != "" && roster.RunID != req.RunID {
			return RosterList{}, typed(ErrConflict)
		}
		run, runErr := s.deps.Runs.GetRun(ctx, roster.RunID)
		if runErr != nil {
			return RosterList{}, typed(runErr)
		}
		if run.ID != roster.RunID {
			return RosterList{}, typed(ErrConflict)
		}
		view := RosterView{Run: run, Version: roster.Version, Members: []MemberView{}}
		for _, m := range roster.Members {
			view.Members = append(view.Members, memberView(m))
		}
		result.Runs = append(result.Runs, view)
	}
	return result, nil
}

func retainedRosterStatus(status string) bool {
	switch status {
	case "active", "stopped", "released", "stopping", "releasing", "failing", "failed":
		return true
	default:
		return false
	}
}
