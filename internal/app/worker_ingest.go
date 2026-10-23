package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/workerwire"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (s *Service) ingestWorkerFrame(workerID string, f workerwire.Frame) error {
	if err := s.requireEventBus(); err != nil {
		return err
	}
	s.workerIngestMu.Lock()
	defer s.workerIngestMu.Unlock()
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
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600) //nolint:gosec // G304: path is the hub-owned session log of a session this connection owns
		if err != nil {
			return err
		}
		offset, seekErr := file.Seek(0, io.SeekEnd)
		if seekErr != nil {
			_ = file.Close()
			return seekErr
		}
		_, err = file.Write(f.Data)
		if err == nil {
			err = file.Sync()
		}
		if err == nil {
			err = s.workerPermissionOutput(row, f, file, offset)
		}
		file.Close()
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"data": f.Data, "stream": f.Stream, "worker_seq": f.Seq})
		// The cursor and the session.output event commit together. The cursor still
		// advances when the turn-output step fails, as it did before; only then is
		// the event not published.
		turnErr := s.workerTurnOutput(row, f)
		if err = s.Store.RecordWorkerOutput(context.Background(), f.Session, f.Seq, string(raw), time.Now().UTC(), turnErr == nil); err != nil {
			return err
		}
		return turnErr
	case "state":
		state := f.State
		// Work-state is what the runtime is doing, separate from the lifecycle:
		// record it before the worker's idle is normalized to running below, or the
		// distinction is lost (CW-20261021-0003).
		if err = s.recordWorkState(row.ID, state, f.At); err != nil {
			return err
		}
		switch state {
		case "exited":
			state = "completed"
			if f.ExitCode != nil && *f.ExitCode != 0 {
				state = "failed"
			}
			// A stop the operator asked for ends as killed, like a local
			// session's, not as the failed that exit 143 would read as.
			if s.stops.requested(f.Session) {
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
// an unchanged value. The caller holds workerIngestMu.
func (s *Service) recordWorkState(sessionID, frameState, frameAt string) error {
	next := workStateOf(frameState)
	if frameState == "exited" {
		delete(s.workStates, sessionID)
		return nil
	}
	if next == "" {
		return nil
	}
	prev, known := s.workStates[sessionID]
	if !known && s.Store != nil {
		if w, ok, err := s.Store.LatestWorkState(context.Background(), sessionID); err == nil && ok {
			prev, known = w.Value, true
		}
	}
	if known && prev == next {
		if s.workStates == nil {
			s.workStates = map[string]string{}
		}
		s.workStates[sessionID] = next
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
	if s.workStates == nil {
		s.workStates = map[string]string{}
	}
	s.workStates[sessionID] = next
	return nil
}
