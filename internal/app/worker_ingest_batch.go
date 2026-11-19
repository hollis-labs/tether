package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// beforeOutputCommit, when set by a test, runs just before a batch commits; an
// error stands in for a crash between the log writes and the commit.
var beforeOutputCommit func() error

// ingestWorkerOutputs ingests a run of output frames the hub drained from one
// worker's queue (CW-20261115-0027). Each frame is checked and written to its
// session log as ingestWorkerFrame does, but every accepted frame's cursor and
// session.output event commit in ONE transaction, and each frame's turn-output
// step runs only after that commit. It returns one error per frame (nil:
// accepted); the hub asks the worker to replay a failed frame's session from
// its stored cursor, as for a single frame.
//
// Durability: a frame is accepted only when its batch commits. A crash before
// the commit loses nothing accepted: the cursors still point before the batch,
// so the worker replays it (its session-log bytes may then appear twice, as
// they may after any failed commit).
func (s *Service) ingestWorkerOutputs(workerID string, frames []workerwire.Frame) []error {
	errs := make([]error, len(frames))
	if err := s.requireEventBus(); err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	// Lock every session in the batch, in a fixed order.
	var ids []string
	for _, f := range frames {
		if !slices.Contains(ids, f.Session) {
			ids = append(ids, f.Session)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		defer s.workerIngest.lock(id)()
	}

	type session struct {
		row        *store.SessionRow
		next       int64
		permission bool
		failed     error
	}
	sessions := map[string]*session{}
	var pending []int
	var outs []store.WorkerOutput
	now := time.Now().UTC()
	for i, f := range frames {
		st := sessions[f.Session]
		if st == nil {
			st = &session{}
			sessions[f.Session] = st
			row, err := s.Store.GetSession(f.Session)
			switch {
			case err != nil:
				st.failed = err
			case row.WorkerID == "" || row.WorkerID != workerID:
				st.failed = fmt.Errorf("worker does not own execution")
			default:
				st.row, st.next, st.permission = row, s.Workers.NextSeq(f.Session), s.workerPermissionSource(row)
			}
		}
		if st.failed != nil {
			errs[i] = st.failed
			continue
		}
		if f.Seq < st.next {
			continue // already accepted
		}
		if f.Seq != st.next {
			st.failed = fmt.Errorf("output sequence gap")
			errs[i] = st.failed
			continue
		}
		permission := f.Stream == "stdout" && st.permission
		var then func(*os.File, int64) error
		if permission {
			row, frame := st.row, f
			then = func(file *os.File, offset int64) error { return s.workerPermissionSpans(row, frame, file, offset) }
		}
		if err := s.sessionLogs.write(st.row.ID, filepath.Join(st.row.Workspace, "logs", "session.log"), f.Data, permission, then); err != nil {
			st.failed = err
			errs[i] = err
			continue
		}
		raw, _ := json.Marshal(map[string]any{"data": f.Data, "stream": f.Stream, "worker_seq": f.Seq})
		outs = append(outs, store.WorkerOutput{SessionID: f.Session, Seq: f.Seq, PayloadJSON: string(raw), At: now})
		pending = append(pending, i)
		st.next++
	}
	if len(pending) == 0 {
		return errs
	}
	err := error(nil)
	if beforeOutputCommit != nil {
		err = beforeOutputCommit()
	}
	if err == nil {
		err = s.Store.RecordWorkerOutputs(context.Background(), outs)
	}
	if err != nil {
		for _, i := range pending {
			errs[i] = err
		}
		return errs
	}
	// Accepted: now, and only now, each frame's turn-output step, in order.
	for _, i := range pending {
		if err := s.workerTurnOutput(sessions[frames[i].Session].row, frames[i]); err != nil {
			errs[i] = err
		}
	}
	return errs
}
