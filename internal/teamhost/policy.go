package teamhost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func (h *Host) ResolveTrust(ctx context.Context, parent teams.Member, slot teams.Slot) (teams.TrustDecision, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target := h.tiers[slot.Definition]
	if target == "" || target == TrustDenied {
		return teams.TrustDeny, nil
	}
	source := h.actors[parent.Actor]
	if parent.Intent != nil {
		source = h.tiers[parent.Intent.Slot.Definition]
	}
	if source == "" || source == TrustDenied {
		return teams.TrustDeny, nil
	}
	if source == TrustApproval || target == TrustApproval {
		return teams.TrustApproval, nil
	}
	return teams.TrustAllow, nil
}
func (h *Host) EmitApproval(ctx context.Context, key string, parent teams.Member, slot teams.Slot) error {
	if key == "" {
		return errors.New("approval: request key required")
	}
	value := struct {
		Parent teams.Member
		Slot   teams.Slot
	}{parent, slot}
	payload, err := encode(value)
	if err != nil {
		return err
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		var old []byte
		err := conn.QueryRowContext(ctx, `SELECT payload FROM team_host_approvals WHERE request_key=?`, key).Scan(&old)
		if err == nil {
			equal, err := same(old, value)
			if err != nil {
				return err
			}
			if !equal {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_approvals(request_key,payload) VALUES(?,?)`, key, payload)
		return err
	})
}
func (h *Host) Evaluate(ctx context.Context, trigger teams.Trigger, m teams.Member) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if trigger.Kind != "manual" {
		return false, fmt.Errorf("trigger %q: %w", trigger.Kind, teams.ErrUnsupported)
	}
	if m.Status != "active" || m.Actor.Validate() != nil {
		return false, teams.ErrUnavailable
	}
	return true, nil
}
func (h *Host) InstallRouting(ctx context.Context, key string, run teams.TeamRun, routing teams.Routing) error {
	if key == "" {
		return errors.New("routing: install key required")
	}
	channel, err := teamstore.ChannelName(run.ID)
	if err != nil {
		return err
	}
	named, err := h.ports.Channels.Name(run.ID)
	if err != nil {
		return err
	}
	if named != channel {
		return fmt.Errorf("routing channel: %w", teams.ErrConflict)
	}
	value := struct {
		Run     teams.TeamRun
		Routing teams.Routing
	}{run, routing}
	payload, err := encode(value)
	if err != nil {
		return err
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		var existing []byte
		var oldKey string
		var removed bool
		err := conn.QueryRowContext(ctx, `SELECT install_key,payload,removed FROM team_host_routing WHERE run_id=?`, run.ID).Scan(&oldKey, &existing, &removed)
		if err == nil {
			if removed {
				return teams.ErrDenied
			}
			equal, err := same(existing, value)
			if err != nil {
				return err
			}
			if oldKey != key || !equal {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var found string
		if err := conn.QueryRowContext(ctx, `SELECT run_id FROM team_runs WHERE run_id=?`, run.ID).Scan(&found); err != nil {
			return notFound(err)
		}
		// The same install key must never acquire another run.
		err = conn.QueryRowContext(ctx, `SELECT run_id FROM team_host_routing WHERE install_key=?`, key).Scan(&found)
		if err == nil {
			return teams.ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_routing(run_id,install_key,payload,channel) VALUES(?,?,?,?)`, run.ID, key, payload, channel)
		return err
	})
}
func (h *Host) RemoveRouting(ctx context.Context, runID string) error {
	if runID == "" {
		return errors.New("routing: run required")
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		// A tombstone also fences an install whose acknowledgement was lost.
		_, err := conn.ExecContext(ctx, `INSERT INTO team_host_routing(run_id,install_key,payload,channel,removed) VALUES(?,?,?, ?,1) ON CONFLICT(run_id) DO UPDATE SET removed=1`, runID, id("removed-routing", runID), []byte(`{}`), "team."+runID)
		return err
	})
}
