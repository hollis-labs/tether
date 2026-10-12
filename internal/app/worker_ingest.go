package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/workerwire"
	"os"
	"path/filepath"
	"time"
)

func (s *Service) ingestWorkerFrame(workerID string, f workerwire.Frame) error {
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
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = file.Write(f.Data)
		if err == nil {
			err = file.Sync()
		}
		file.Close()
		if err != nil {
			return err
		}
		if _, err = s.Store.DB().Exec(`INSERT INTO session_worker_seq(session_id,last_seq) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET last_seq=excluded.last_seq`, f.Session, f.Seq); err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"data": f.Data, "stream": f.Stream, "worker_seq": f.Seq})
		return s.publishWorkerEvent(f.Session, "session.output", raw)
	case "state":
		state := f.State
		if state == "exited" {
			state = "completed"
			if f.ExitCode != nil && *f.ExitCode != 0 {
				state = "failed"
			}
		} else if state == "idle" {
			state = "running"
		}
		if err = s.Store.UpdateSessionState(f.Session, state, int(row.PID.Int64), f.ExitCode); err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"from": row.State, "to": state, "exit_code": f.ExitCode})
		return s.publishWorkerEvent(f.Session, "session.state_changed", raw)
	}
	return nil
}
func (s *Service) publishWorkerEvent(id, kind string, raw []byte) error {
	if s.Bus != nil {
		return s.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: id, Kind: kind, At: time.Now().UTC(), PayloadJSON: string(raw)})
	}
	_, _, err := s.Store.InsertEvent(events.ScopeSession, id, kind, string(raw))
	return err
}
