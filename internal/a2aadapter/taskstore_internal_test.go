package a2aadapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/hollis-labs/tether/internal/store"
)

func newDurable(t *testing.T, owner string) *durableTaskStore {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &durableTaskStore{p: db, bindingID: "b", owner: owner}
}

func task(id, ctx string, state a2a.TaskState) *a2a.Task {
	return &a2a.Task{ID: a2a.TaskID(id), ContextID: ctx, Status: a2a.TaskStatus{State: state}}
}

func TestDurableTaskStore_CreateGetUpdateErrors(t *testing.T) {
	s := newDurable(t, "")
	ctx := context.Background()
	v, err := s.Create(ctx, task("t1", "c", a2a.TaskStateSubmitted))
	if err != nil || v != 1 {
		t.Fatalf("Create = (%d, %v)", v, err)
	}
	if _, err := s.Create(ctx, task("t1", "c", a2a.TaskStateSubmitted)); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("duplicate Create error = %v, want ErrTaskAlreadyExists", err)
	}
	got, err := s.Get(ctx, "t1")
	if err != nil || got.Version != 1 || got.Task.ContextID != "c" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task("t1", "c", a2a.TaskStateWorking), PrevVersion: 1}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task("t1", "c", a2a.TaskStateWorking), PrevVersion: 1}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("stale Update error = %v, want ErrConcurrentModification", err)
	}
	if _, err := s.Update(ctx, &taskstore.UpdateRequest{Task: task("nope", "c", a2a.TaskStateWorking)}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Update of a missing task error = %v, want ErrTaskNotFound", err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("Get of a missing task error = %v, want ErrTaskNotFound", err)
	}
}

// With no owner (the adapter's configuration) List is refused, as the SDK's
// in-memory store refuses it.
func TestDurableTaskStore_ListWithoutAnOwnerIsUnauthenticated(t *testing.T) {
	if _, err := newDurable(t, "").List(context.Background(), &a2a.ListTasksRequest{}); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Fatalf("List error = %v, want ErrUnauthenticated", err)
	}
}

func TestDurableTaskStore_ListFiltersAndPages(t *testing.T) {
	s := newDurable(t, "me")
	ctx := context.Background()
	for _, tk := range []*a2a.Task{
		task("t1", "c1", a2a.TaskStateWorking),
		task("t2", "c1", a2a.TaskStateCompleted),
		task("t3", "c2", a2a.TaskStateWorking),
	} {
		if _, err := s.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List(ctx, &a2a.ListTasksRequest{})
	if err != nil || all.TotalSize != 3 || len(all.Tasks) != 3 {
		t.Fatalf("List all = %+v, %v", all, err)
	}
	if all.Tasks[0].ID != "t3" {
		t.Errorf("first = %s, want the most recently updated (t3)", all.Tasks[0].ID)
	}
	byCtx, _ := s.List(ctx, &a2a.ListTasksRequest{ContextID: "c1"})
	if byCtx.TotalSize != 2 {
		t.Errorf("context filter matched %d, want 2", byCtx.TotalSize)
	}
	byState, _ := s.List(ctx, &a2a.ListTasksRequest{Status: a2a.TaskStateWorking})
	if byState.TotalSize != 2 {
		t.Errorf("state filter matched %d, want 2", byState.TotalSize)
	}

	first, err := s.List(ctx, &a2a.ListTasksRequest{PageSize: 2})
	if err != nil || len(first.Tasks) != 2 || first.NextPageToken == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := s.List(ctx, &a2a.ListTasksRequest{PageSize: 2, PageToken: first.NextPageToken})
	if err != nil || len(second.Tasks) != 1 || second.NextPageToken != "" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	if second.Tasks[0].ID != "t1" {
		t.Errorf("second page holds %s, want t1 (no task repeated or skipped)", second.Tasks[0].ID)
	}
	if _, err := s.List(ctx, &a2a.ListTasksRequest{PageSize: 101}); !errors.Is(err, a2a.ErrInvalidRequest) {
		t.Errorf("oversized page error = %v, want ErrInvalidRequest", err)
	}
}
