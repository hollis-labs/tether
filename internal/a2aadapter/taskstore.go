package a2aadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/hollis-labs/tether/internal/store"
)

// TaskPersistence is the durable storage the adapter keeps its A2A task
// records in. *store.Store satisfies it. Without one, the A2A SDK's default
// in-memory task store is used and every task is lost when the process
// restarts.
type TaskPersistence interface {
	CreateA2ATask(t store.A2ATask) (int64, error)
	UpdateA2ATask(t store.A2ATask, prevVersion int64) (int64, error)
	GetA2ATask(bindingID, taskID string) (store.A2ATask, error)
	ListA2ATasks(bindingID, owner, contextID, state string) ([]store.A2ATask, error)
	ListA2ATasksInStates(states ...string) ([]store.A2ATask, error)
}

// Option configures NewAdapter.
type Option func(*Adapter)

// WithTaskPersistence keeps A2A task records in p, so a peer's tasks survive a
// restart, and makes the adapter sweep tasks a restart left in flight (see
// ReconcileInterrupted).
func WithTaskPersistence(p TaskPersistence) Option {
	return func(a *Adapter) { a.persistence = p }
}

// durableTaskStore is a taskstore.Store over TaskPersistence. It mirrors the
// SDK's in-memory store (same errors, versioning and owner scoping) so a
// handler cannot tell the two apart, except that this one survives a restart.
// It is scoped to one binding: a peer of one binding has never been able to
// read another's tasks, and sharing a table must not change that.
type durableTaskStore struct {
	p         TaskPersistence
	bindingID string
	// owner is the SDK task-owner identity. The adapter runs without an
	// authenticator, so it is "" (as with the in-memory store's default), and
	// an owner-less List is refused, as the SDK's is.
	owner string
}

var _ taskstore.Store = (*durableTaskStore)(nil)

func encodeTask(bindingID, owner string, task *a2a.Task) (store.A2ATask, error) {
	js, err := json.Marshal(task)
	if err != nil {
		return store.A2ATask{}, fmt.Errorf("encode a2a task: %w", err)
	}
	return store.A2ATask{
		BindingID: bindingID,
		TaskID:    string(task.ID),
		Owner:     owner,
		ContextID: task.ContextID,
		State:     string(task.Status.State),
		TaskJSON:  js,
	}, nil
}

func decodeTask(rec store.A2ATask) (*a2a.Task, error) {
	var t a2a.Task
	if err := json.Unmarshal(rec.TaskJSON, &t); err != nil {
		return nil, fmt.Errorf("decode a2a task %s: %w", rec.TaskID, err)
	}
	return &t, nil
}

func (s *durableTaskStore) Create(_ context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	rec, err := encodeTask(s.bindingID, s.owner, task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	v, err := s.p.CreateA2ATask(rec)
	if errors.Is(err, store.ErrA2ATaskExists) {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(v), nil
}

func (s *durableTaskStore) Update(_ context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	rec, err := encodeTask(s.bindingID, s.owner, req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	v, err := s.p.UpdateA2ATask(rec, int64(req.PrevVersion))
	switch {
	case errors.Is(err, store.ErrA2ATaskNotFound):
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	case errors.Is(err, store.ErrA2ATaskConflict):
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	case err != nil:
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(v), nil
}

func (s *durableTaskStore) Get(_ context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	rec, err := s.p.GetA2ATask(s.bindingID, string(taskID))
	if errors.Is(err, store.ErrA2ATaskNotFound) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	if rec.Owner != s.owner {
		return nil, a2a.ErrTaskNotFound
	}
	task, err := decodeTask(rec)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(rec.Version), User: rec.Owner}, nil
}

// List mirrors the SDK's in-memory store, which refuses an unauthenticated
// (owner "") list.
func (s *durableTaskStore) List(_ context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if s.owner == "" {
		return nil, a2a.ErrUnauthenticated
	}
	const defaultPageSize = 50
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	} else if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page size must be between 1 and 100 inclusive, got %d: %w", pageSize, a2a.ErrInvalidRequest)
	}
	recs, err := s.p.ListA2ATasks(s.bindingID, s.owner, req.ContextID, string(req.Status))
	if err != nil {
		return nil, err
	}
	var matched []store.A2ATask
	for _, rec := range recs {
		if req.StatusTimestampAfter != nil {
			t, err := decodeTask(rec)
			if err != nil {
				return nil, err
			}
			if t.Status.Timestamp != nil && t.Status.Timestamp.Before(*req.StatusTimestampAfter) {
				continue
			}
		}
		matched = append(matched, rec)
	}
	page := matched
	if req.PageToken != "" {
		cursorAt, cursorID, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		start := len(matched)
		for i, rec := range matched {
			if rec.UpdatedAt.Before(cursorAt) || (rec.UpdatedAt.Equal(cursorAt) && rec.TaskID < cursorID) {
				start = i
				break
			}
		}
		page = matched[start:]
	}
	var next string
	if pageSize < len(page) {
		last := page[pageSize-1]
		next = encodePageToken(last.UpdatedAt, last.TaskID)
		page = page[:pageSize]
	}
	out := make([]*a2a.Task, 0, len(page))
	for _, rec := range page {
		t, err := decodeTask(rec)
		if err != nil {
			return nil, err
		}
		historyLength := 100
		if req.HistoryLength != nil {
			historyLength = *req.HistoryLength
		}
		if historyLength <= 0 {
			t.History = []*a2a.Message{}
		} else if len(t.History) > historyLength {
			t.History = t.History[len(t.History)-historyLength:]
		}
		if !req.IncludeArtifacts {
			t.Artifacts = nil
		}
		out = append(out, t)
	}
	return &a2a.ListTasksResponse{Tasks: out, TotalSize: len(matched), PageSize: pageSize, NextPageToken: next}, nil
}

func encodePageToken(at time.Time, taskID string) string {
	return base64.URLEncoding.EncodeToString(fmt.Appendf(nil, "%s_%s", at.Format(time.RFC3339Nano), taskID))
}

func decodePageToken(tok string) (time.Time, string, error) {
	raw, err := base64.URLEncoding.DecodeString(tok)
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	at, id, ok := strings.Cut(string(raw), "_")
	if !ok {
		return time.Time{}, "", a2a.ErrParseError
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	return t, id, nil
}

// interruptedMessage is what a peer reads on a task a restart cut off.
const interruptedMessage = "interrupted: Tether restarted while this task was waiting for a consumer transition; resubmit it"

// ReconcileInterrupted moves every task the last process left in submitted or
// working to failed, and reports how many it moved. Such a task had an
// Execute call blocked on a consumer transition; that wait lived only in the
// process that died, so nothing will ever resolve the task, and leaving it
// 'working' would tell the peer it is still in progress. The delegated
// message itself was durably relayed through messaging and is untouched.
// Terminal tasks, and input-required ones (whose Execute had already returned
// before the restart), are left as they were. Call it once at startup, before
// serving. It does nothing without WithTaskPersistence.
func (a *Adapter) ReconcileInterrupted(ctx context.Context) (int, error) {
	if a.persistence == nil {
		return 0, nil
	}
	recs, err := a.persistence.ListA2ATasksInStates(
		string(a2a.TaskStateSubmitted), string(a2a.TaskStateWorking))
	if err != nil {
		return 0, fmt.Errorf("list in-flight a2a tasks: %w", err)
	}
	moved := 0
	var errs []error
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return moved, err
		}
		task, err := decodeTask(rec)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		now := time.Now().UTC()
		task.Status = a2a.TaskStatus{
			State:     a2a.TaskStateFailed,
			Message:   a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(interruptedMessage)),
			Timestamp: &now,
		}
		next, err := encodeTask(rec.BindingID, rec.Owner, task)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := a.persistence.UpdateA2ATask(next, rec.Version); err != nil {
			if errors.Is(err, store.ErrA2ATaskConflict) {
				continue // something else already moved it
			}
			errs = append(errs, err)
			continue
		}
		moved++
	}
	return moved, errors.Join(errs...)
}
