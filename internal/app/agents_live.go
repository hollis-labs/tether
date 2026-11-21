package app

import (
	"context"
	"os"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/agentlive"
	"github.com/hollis-labs/tether/internal/store"
)

// AgentsLive builds the tether_agents_live view (CW-20261121-0114): running and
// detached Tether sessions plus kit-booted seats, with presence evidence from
// session runtimes, open mail-watch streams (watch, may be nil), live runtime
// bindings and seat registrations. Read-only.
func (s *Service) AgentsLive(ctx context.Context, q agentlive.Query, watch *agentlive.Tracker) (agentlive.Response, error) {
	now := time.Now().UTC()
	in := agentlive.Inputs{Now: now, Presence: map[string]agentlive.Evidence{}}
	cat := s.cat()
	agentURN := func(id string) string {
		if cat != nil {
			if a, ok := cat.Agents[id]; ok && a.URN != "" {
				return a.URN
			}
		}
		return "msg://agent/agent-mux/" + id
	}
	hostName, _ := os.Hostname()
	workerLabels := map[string]string{}
	hostOf := func(workerID string) string {
		if workerID == "" {
			return hostName
		}
		if l, ok := workerLabels[workerID]; ok {
			return l
		}
		var label string
		if err := s.Store.DB().QueryRowContext(ctx, `SELECT label FROM workers WHERE id=?`, workerID).Scan(&label); err != nil {
			label = ""
		}
		workerLabels[workerID] = label
		return label
	}
	declared := func(urn string) *agentlive.Declared {
		d, ok, err := s.Store.GetSeatStatus(ctx, urn)
		if err != nil || !ok || (!d.ExpiresAt.IsZero() && d.ExpiresAt.Before(now)) {
			return nil
		}
		return &agentlive.Declared{State: string(d.State), Reason: d.Reason, Ref: d.Ref, UpdatedAt: d.UpdatedAt}
	}

	if q.Kind != agentlive.KindKitSeat {
		for _, state := range []string{"running", "detached"} {
			rows, err := s.Store.ListSessions(store.ListSessionsOptions{State: state, Limit: agentlive.MaxLimit})
			if err != nil {
				return agentlive.Response{}, err
			}
			for _, r := range rows {
				urn := agentURN(r.LogicalAgentID)
				si := agentlive.SessionInput{ID: r.ID, AgentURN: urn, Name: r.LogicalAgentID, Host: hostOf(r.WorkerID), RawState: r.State,
					WorkstreamID: r.WorkstreamID.String, TaskID: r.TaskID.String, RunID: r.RunID.String, Declared: declared(urn)}
				if h, ok := s.RuntimeHealth(r.ID); ok {
					si.Alive = h.Health.Alive
					switch h.Health.State {
					case agentsessions.LiveStateProcessing:
						si.Live = agentlive.RawBusy
					case agentsessions.LiveStateIdle:
						si.Live = agentlive.RawIdle
					case agentsessions.LiveStateStopped:
						// Stopping: not an activity observation.
					}
				}
				if obs, err := s.SessionHealthObservations(ctx, r.ID); err == nil && obs.LastActivityAt != nil {
					si.LastActivity = *obs.LastActivityAt
				}
				in.Sessions = append(in.Sessions, si)
			}
		}
	}

	seats, err := s.Store.ListKitSeats(ctx, now.Add(-store.KitSeatRetention), agentlive.MaxLimit)
	if err != nil {
		return agentlive.Response{}, err
	}
	for _, k := range seats {
		in.Seats = append(in.Seats, agentlive.SeatInput{URN: k.AgentURN, Session: k.Session, RunID: k.RunID, TaskID: k.TaskID,
			WorkstreamID: k.WorkstreamID, Host: k.Host, Name: k.DisplayName, Summary: k.Summary, LastRegisteredAt: k.LastSeenAt, Declared: declared(k.AgentURN)})
	}

	if watch != nil {
		for urn, ev := range watch.Snapshot(now, store.KitSeatRetention) {
			in.Presence[urn] = ev
		}
	}
	if s.Registry != nil {
		urns := map[string]bool{}
		for _, si := range in.Sessions {
			urns[si.AgentURN] = true
		}
		for _, k := range in.Seats {
			urns[k.URN] = true
		}
		for urn := range urns {
			bs, err := s.Registry.ListBindingsForTarget(ctx, urn)
			if err != nil {
				continue
			}
			for _, b := range bs {
				if b.RevokedAt != nil || (b.LeaseExpiresAt != nil && !b.LeaseExpiresAt.After(now)) {
					continue
				}
				if cur := in.Presence[urn]; b.UpdatedAt.After(cur.At) {
					in.Presence[urn] = agentlive.Evidence{At: b.UpdatedAt, Source: "binding"}
				}
			}
		}
	}
	return agentlive.Build(in, q), nil
}
