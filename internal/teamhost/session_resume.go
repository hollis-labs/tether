package teamhost

import (
	"context"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh/teams"
)

// RetainedSessionRecoverer recovers custody under the original enrollment.
// Lost execution may atomically remap current membership to a new session;
// historical delivery targets remain unchanged. The ordinary Sessions interface
// remains sufficient for hosts that have no recovery implementation.
type RetainedSessionRecoverer interface {
	Recover(context.Context, string, string) error
}

// RecoverSessions is an explicitly scheduled boot pass, not a constructor side
// effect. It resumes active retained members, including originally fresh actors
// whose original enrollment is still valid. No provision/enroll port is called.
func (h *Host) RecoverSessions(ctx context.Context) error {
	port, ok := h.ports.Sessions.(RetainedSessionRecoverer)
	if !ok {
		return ErrSessionUnavailable
	}
	rows, err := h.db.QueryContext(ctx, `SELECT intent_key FROM team_host_intents WHERE member IS NOT NULL AND tombstone='' AND cleaned=0 AND dead=0 ORDER BY sequence`)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, key := range keys {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		err = h.store.WithLease(ctx, "resume:"+key, func(leaseCtx context.Context) error {
			state, err := loadIntent(leaseCtx, h.db, key)
			if err != nil {
				return err
			}
			if state.tombstone != "" || state.cleaned || state.member == nil || state.enrollment == nil {
				return teams.ErrDenied
			}
			m, e := state.member, state.enrollment
			if !m.Enrolled || m.Actor != e.Actor || m.AgentID != e.AgentID || m.Kind != e.Kind || m.SessionID == "" {
				return teams.ErrConflict
			}
			if m.Status != "active" {
				return nil
			}
			if err = h.requestTeam(leaseCtx, state.request); err != nil {
				return err
			}
			runID := state.request.RunID
			if runID == "" {
				var launchKey string
				if err := h.db.QueryRowContext(leaseCtx, `SELECT launch_key FROM team_host_launch_intents WHERE intent_key=?`, key).Scan(&launchKey); err != nil {
					return err
				}
				record, err := h.store.GetLaunch(leaseCtx, launchKey)
				if err != nil {
					return err
				}
				runID = record.Run.ID
			}
			run, err := h.store.GetRun(leaseCtx, runID)
			if err != nil {
				return err
			}
			if run.Run.Status.Terminal() {
				return nil
			}
			roster, err := h.store.Snapshot(leaseCtx, runID)
			if err != nil {
				return err
			}
			for _, current := range roster.Members {
				if current.ID == m.ID && current.Status == "active" && current.Actor == m.Actor && current.SessionID == m.SessionID && current.AgentID == m.AgentID && current.Enrolled {
					return port.Recover(leaseCtx, key, m.SessionID)
				}
			}
			return nil // a removed member is not pending work
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("retained member %s: %w", key, err))
		}
	}
	return errors.Join(failures...)
}
