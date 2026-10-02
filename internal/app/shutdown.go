package app

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/session"
)

// ShutdownStopReason is the `reason` of the terminal session.state_changed
// event of a session that ended because the daemon was stopped on purpose.
// Its row state is `killed`. A crash leaves no such record: the next start's
// sweep fails those sessions with exit_code -1 (CW-20260912-0086).
const ShutdownStopReason = "daemon-shutdown"

// DrainSessions is the daemon's graceful-shutdown drain. Before waiting for
// its sessions to end, it records why they are ending: every session still
// live is marked as stopped with ShutdownStopReason, so an exit observed
// during the drain is recorded `killed` with that reason rather than as a
// failure. On a systemd host with the default KillMode=control-group the
// agents receive the same SIGTERM as the daemon and exit during the drain.
//
// It does not signal sessions itself. One that has not exited when ctx ends
// stays non-terminal: it may still be running (a daemon outside a unit that
// kills its children), and the next start's sweep then spares it if its own
// process is alive or fails it with exit_code -1 if not.
//
// It publishes one daemon.shutdown_sessions_ended event naming both sets and
// returns the drain's error.
func (s *Service) DrainSessions(ctx context.Context) error {
	var live []string
	for _, info := range s.Manager.List() {
		if info.State == agentsessions.StateLaunching || info.State == agentsessions.StateRunning {
			live = append(live, info.ID)
		}
	}
	for _, id := range live {
		s.stops.markWithReason(id, ShutdownStopReason)
	}

	err := s.Manager.Shutdown(ctx)

	var ended, stillRunning []string
	for _, id := range live {
		row, gerr := s.Store.GetSession(id)
		if gerr == nil {
			switch session.State(row.State) {
			case session.StateCompleted, session.StateFailed, session.StateKilled:
				ended = append(ended, id)
				continue
			default:
			}
		}
		stillRunning = append(stillRunning, id)
	}
	s.reportShutdownSessions(ended, stillRunning)
	return err
}

// reportShutdownSessions logs what the drain did and publishes it as one
// daemon.shutdown_sessions_ended event.
func (s *Service) reportShutdownSessions(ended, stillRunning []string) {
	if len(ended) == 0 && len(stillRunning) == 0 {
		return
	}
	if len(ended) > 0 {
		log.Printf("app: shutdown ended %d session(s) as killed (%s): %s", len(ended), ShutdownStopReason, strings.Join(ended, ", "))
	}
	if len(stillRunning) > 0 {
		log.Printf("app: shutdown left %d session(s) that had not exited; the next start's sweep settles them: %s", len(stillRunning), strings.Join(stillRunning, ", "))
	}
	if s.Bus == nil {
		return
	}
	payload, err := json.Marshal(shutdownSessionsPayload{
		Ended:                  len(ended),
		EndedSessionIDs:        nonNil(ended),
		StillRunning:           len(stillRunning),
		StillRunningSessionIDs: nonNil(stillRunning),
	})
	if err != nil {
		return
	}
	ctx, cancel := s.outputPersistenceContext()
	defer cancel()
	_ = s.Bus.Publish(ctx, events.Event{
		Scope:       events.ScopeDaemon,
		Kind:        events.KindDaemonShutdownSessionsEnded,
		PayloadJSON: string(payload),
	})
}

type shutdownSessionsPayload struct {
	Ended                  int      `json:"ended"`
	EndedSessionIDs        []string `json:"ended_session_ids"`
	StillRunning           int      `json:"still_running"`
	StillRunningSessionIDs []string `json:"still_running_session_ids"`
}
