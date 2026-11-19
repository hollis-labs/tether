package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/workerwire"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func (s *Service) ingestWorkerFrame(workerID string, f workerwire.Frame) error {
	if err := s.requireEventBus(); err != nil {
		return err
	}
	defer s.workerIngest.lock(f.Session)()
	row, err := s.Store.GetSession(f.Session)
	if err != nil {
		return err
	}
	if row.WorkerID == "" || row.WorkerID != workerID {
		return fmt.Errorf("worker does not own execution")
	}
	switch f.T {
	case "output":
		last := s.Workers.NextSeq(f.Session) - 1
		if f.Seq <= last {
			return nil
		}
		if f.Seq != last+1 {
			return fmt.Errorf("output sequence gap")
		}
		path := filepath.Join(row.Workspace, "logs", "session.log")
		// A codex permission-source session's parser keeps byte spans of the log
		// in SQLite, so its stdout is synced before the spans commit; every other
		// frame is group-committed (worker_session_log.go).
		permission := f.Stream == "stdout" && s.workerPermissionSource(row)
		var then func(*os.File, int64) error
		if permission {
			then = func(file *os.File, offset int64) error { return s.workerPermissionSpans(row, f, file, offset) }
		}
		if err = s.sessionLogs.write(row.ID, path, f.Data, permission, then); err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"data": f.Data, "stream": f.Stream, "worker_seq": f.Seq})
		// The cursor and the session.output event commit first, together. The turn-output
		// step keeps in-memory state (a partial result line) and publishes turn events, so
		// it must not run until the frame is durably accepted: a frame whose commit failed
		// is replayed from the old cursor and would otherwise be applied twice.
		if err = s.Store.RecordWorkerOutput(context.Background(), f.Session, f.Seq, string(raw), time.Now().UTC()); err != nil {
			return err
		}
		return s.workerTurnOutput(row, f)
	case "state":
		state := f.State
		// Work-state is what the runtime is doing, separate from the lifecycle:
		// record it before the worker's idle is normalized to running below, or the
		// distinction is lost (CW-20261021-0003).
		if err = s.recordWorkState(row.ID, state, f.At); err != nil {
			return err
		}
		var endReason string
		switch state {
		case "exited":
			s.sessionLogs.closeSession(row.ID)
			s.dropStdioTap(row.ID, nil)
			state = "completed"
			if f.ExitCode != nil && *f.ExitCode != 0 {
				state = "failed"
			}
			// A stop the operator asked for ends as killed, like a local
			// session's, not as the failed that exit 143 would read as.
			stopRequested := s.stops.requested(f.Session)
			endReason = s.workerEndReason(f.Session, stopRequested, s.stops.reason(f.Session))
			if stopRequested {
				s.stops.clear(f.Session)
				state = "killed"
			}
		case "idle":
			state = "running"
		}
		if err = s.Store.UpdateSessionState(f.Session, state, int(row.PID.Int64), f.ExitCode); err != nil {
			return err
		}
		if state == "completed" || state == "failed" || state == "killed" {
			s.releaseWorkerIdentity(row.ID)
			if s.Registry != nil {
				_, _ = s.Registry.RevokeSessionBindings(context.Background(), row.ID)
			}
		}
		payload := map[string]any{"from": row.State, "to": state, "exit_code": f.ExitCode}
		if endReason != "" {
			payload["reason"] = endReason
		}
		if f.Error != "" {
			payload["detail"] = f.Error
		}
		raw, _ := json.Marshal(payload)
		return s.publishWorkerEvent(f.Session, "session.state_changed", raw)
	}
	return nil
}
func (s *Service) publishWorkerEvent(id, kind string, raw []byte) error {
	if err := s.requireEventBus(); err != nil {
		return err
	}
	return s.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: id, Kind: kind, At: time.Now().UTC(), PayloadJSON: string(raw)})
}

// workStateOf maps a worker state frame to a work-state value. Frames that do
// not describe the runtime's activity (exited, anything unknown) map to "".
func workStateOf(frameState string) string {
	switch frameState {
	case "idle":
		return "idle"
	case "running":
		return "processing"
	}
	return ""
}

// recordWorkState publishes session.work_state_changed when the session's
// work-state really changed. The previous value is kept in memory and, after a
// restart, read from the last recorded event, so a restart does not re-announce
// an unchanged value. The caller holds the session's ingest lock.
func (s *Service) recordWorkState(sessionID, frameState, frameAt string) error {
	next := workStateOf(frameState)
	if frameState == "exited" {
		s.setWorkState(sessionID, "")
		return nil
	}
	if next == "" {
		return nil
	}
	s.workerStateMu.Lock()
	prev, known := s.workStates[sessionID]
	s.workerStateMu.Unlock()
	if !known && s.Store != nil {
		if w, ok, err := s.Store.LatestWorkState(context.Background(), sessionID); err == nil && ok {
			prev, known = w.Value, true
		}
	}
	if known && prev == next {
		s.setWorkState(sessionID, next)
		return nil
	}
	from := "unknown"
	if known {
		from = prev
	}
	at := time.Now().UTC()
	if t, err := time.Parse(time.RFC3339Nano, frameAt); err == nil && !t.After(at) {
		at = t.UTC()
	}
	raw, _ := json.Marshal(map[string]string{"from": from, "to": next, "at": at.Format(time.RFC3339Nano)})
	if err := s.publishWorkerEvent(sessionID, events.KindSessionWorkStateChanged, raw); err != nil {
		return err
	}
	s.setWorkState(sessionID, next)
	return nil
}

// setWorkState records (or, with "", forgets) a session's last work-state.
func (s *Service) setWorkState(sessionID, value string) {
	s.workerStateMu.Lock()
	defer s.workerStateMu.Unlock()
	if value == "" {
		delete(s.workStates, sessionID)
		return
	}
	if s.workStates == nil {
		s.workStates = map[string]string{}
	}
	s.workStates[sessionID] = value
}

// sessionLocks is a lock per session id, dropped when nobody holds or waits
// on it.
type sessionLocks struct {
	mu    sync.Mutex
	locks map[string]*sessionLock
}

type sessionLock struct {
	sync.Mutex
	refs int
}

// lock locks id and returns its unlock.
func (l *sessionLocks) lock(id string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*sessionLock{}
	}
	sl := l.locks[id]
	if sl == nil {
		sl = &sessionLock{}
		l.locks[id] = sl
	}
	sl.refs++
	l.mu.Unlock()
	sl.Lock()
	return func() {
		sl.Unlock()
		l.mu.Lock()
		if sl.refs--; sl.refs == 0 {
			delete(l.locks, id)
		}
		l.mu.Unlock()
	}
}
