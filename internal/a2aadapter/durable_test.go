package a2aadapter_test

// durable_test.go — CW-20260930-0063: A2A task records survive a daemon
// restart, and a task a restart cut off in flight is failed, not left
// looking alive. A "restart" here is a second *store.Store and Adapter opened
// on the same database file, which is exactly what a new process does.

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/hollis-labs/tether/internal/a2aadapter"
	"github.com/hollis-labs/tether/internal/store"
)

// process is one "run" of the daemon's A2A surface over dbPath.
type process struct {
	db      *store.Store
	adapter *a2aadapter.Adapter
	baseURL string
}

func startProcess(t *testing.T, dbPath string, durable bool, bindings ...a2aadapter.AgentBinding) *process {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	srv := httptest.NewServer(nil)
	for i := range bindings {
		bindings[i].BaseURL = srv.URL
	}
	var opts []a2aadapter.Option
	if durable {
		opts = append(opts, a2aadapter.WithTaskPersistence(db))
	}
	adapter, err := a2aadapter.NewAdapter(a2aadapter.Config{Bindings: bindings}, db.MessagingStore(), opts...)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	srv.Config.Handler = adapter.Tether()
	p := &process{db: db, adapter: adapter, baseURL: srv.URL}
	t.Cleanup(func() { srv.Close(); _ = db.Close() })
	return p
}

// stop ends the process the way a restart does: the server goes away and the
// DB handle closes, while its in-memory state is simply gone.
func (p *process) stop(t *testing.T) {
	t.Helper()
	_ = p.db.Close()
}

func timedOutTask(t *testing.T, p *process, bindingID string) *a2a.Task {
	t.Helper()
	client := newClient(t, resolveCard(t, p.baseURL, bindingID))
	result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("work")),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := result.(*a2a.Task)
	if !ok {
		t.Fatalf("result = %T, want *a2a.Task", result)
	}
	return task
}

func getTask(t *testing.T, p *process, bindingID string, id a2a.TaskID) (*a2a.Task, error) {
	t.Helper()
	client := newClient(t, resolveCard(t, p.baseURL, bindingID))
	return client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
}

func TestDurableTasks_SurviveARestart(t *testing.T) {
	binding := func() a2aadapter.AgentBinding {
		return a2aadapter.AgentBinding{ID: "b", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 50 * time.Millisecond}
	}
	dbPath := filepath.Join(t.TempDir(), "restart.db")

	first := startProcess(t, dbPath, true, binding())
	sent := timedOutTask(t, first, "b")
	if sent.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("setup: state = %s, want input_required", sent.Status.State)
	}
	first.stop(t)

	second := startProcess(t, dbPath, true, binding())
	got, err := getTask(t, second, "b", sent.ID)
	if err != nil {
		t.Fatalf("GetTask after restart: %v (the task must survive)", err)
	}
	if got.ID != sent.ID || got.ContextID != sent.ContextID || got.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task after restart = %+v, want id %s context %s state input_required", got, sent.ID, sent.ContextID)
	}
	if len(got.History) == 0 {
		t.Errorf("history was lost across the restart")
	}
}

// Control: the same sequence without persistence is what the bug was.
func TestDurableTasks_WithoutPersistenceATaskIsLostOnRestart(t *testing.T) {
	binding := func() a2aadapter.AgentBinding {
		return a2aadapter.AgentBinding{ID: "b", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 50 * time.Millisecond}
	}
	dbPath := filepath.Join(t.TempDir(), "volatile.db")
	first := startProcess(t, dbPath, false, binding())
	sent := timedOutTask(t, first, "b")
	first.stop(t)

	second := startProcess(t, dbPath, false, binding())
	if _, err := getTask(t, second, "b", sent.ID); err == nil {
		t.Fatal("task survived a restart without persistence; the control no longer reproduces the bug")
	}
}

func TestDurableTasks_AnInFlightTaskIsFailedNotLeftWorking(t *testing.T) {
	binding := func() a2aadapter.AgentBinding {
		return a2aadapter.AgentBinding{ID: "b", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 30 * time.Second}
	}
	dbPath := filepath.Join(t.TempDir(), "inflight.db")
	first := startProcess(t, dbPath, true, binding())

	// Start delegated work and leave it blocked awaiting a transition.
	client := newClient(t, resolveCard(t, first.baseURL, "b"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = client.SendMessage(ctx, &a2a.SendMessageRequest{
			Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("blocked work")),
		})
	}()
	var inflight store.A2ATask
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs, err := first.db.ListA2ATasksInStates(string(a2a.TaskStateWorking))
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 1 {
			inflight = recs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delegated task never reached working")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The process dies here: nothing resolves the task, and its wait is gone.
	second := startProcess(t, dbPath, true, binding())
	n, err := second.adapter.ReconcileInterrupted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("ReconcileInterrupted = (%d, %v), want (1, nil)", n, err)
	}
	cancel()
	<-done

	got, err := getTask(t, second, "b", a2a.TaskID(inflight.TaskID))
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state after restart = %s, want failed", got.Status.State)
	}
	if got.Status.Message == nil || !strings.Contains(got.Status.Message.Parts[0].Text(), "interrupted") {
		t.Errorf("status message = %+v, want it to say the task was interrupted", got.Status.Message)
	}

	// A second sweep has nothing left to do.
	if n, err := second.adapter.ReconcileInterrupted(context.Background()); err != nil || n != 0 {
		t.Fatalf("second ReconcileInterrupted = (%d, %v), want (0, nil)", n, err)
	}
}

func TestDurableTasks_SweepLeavesSettledTasksAlone(t *testing.T) {
	binding := a2aadapter.AgentBinding{ID: "b", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 50 * time.Millisecond}
	dbPath := filepath.Join(t.TempDir(), "settled.db")
	first := startProcess(t, dbPath, true, binding)
	sent := timedOutTask(t, first, "b") // input_required: its Execute had already returned
	first.stop(t)

	second := startProcess(t, dbPath, true, binding)
	if n, err := second.adapter.ReconcileInterrupted(context.Background()); err != nil || n != 0 {
		t.Fatalf("ReconcileInterrupted = (%d, %v), want (0, nil): input_required is not in flight", n, err)
	}
	got, err := getTask(t, second, "b", sent.ID)
	if err != nil || got.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task = %+v, %v; want it untouched in input_required", got, err)
	}
}

// One table serves every binding; a peer of one must still not see another's.
func TestDurableTasks_AreScopedToTheirBinding(t *testing.T) {
	mk := func(id string) a2aadapter.AgentBinding {
		return a2aadapter.AgentBinding{ID: id, TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 50 * time.Millisecond}
	}
	p := startProcess(t, filepath.Join(t.TempDir(), "scoped.db"), true, mk("alpha"), mk("beta"))
	sent := timedOutTask(t, p, "alpha")

	if _, err := getTask(t, p, "alpha", sent.ID); err != nil {
		t.Fatalf("GetTask on its own binding: %v", err)
	}
	if _, err := getTask(t, p, "beta", sent.ID); err == nil {
		t.Fatal("binding beta read a task that belongs to binding alpha")
	}
}

func TestStore_A2ATaskVersioningAndOwnership(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec := store.A2ATask{BindingID: "b", TaskID: "t1", Owner: "u", ContextID: "c", State: "TASK_STATE_WORKING", TaskJSON: []byte(`{}`)}

	if v, err := db.CreateA2ATask(rec); err != nil || v != 1 {
		t.Fatalf("CreateA2ATask = (%d, %v), want (1, nil)", v, err)
	}
	if _, err := db.CreateA2ATask(rec); !errors.Is(err, store.ErrA2ATaskExists) {
		t.Fatalf("duplicate create error = %v, want ErrA2ATaskExists", err)
	}
	if v, err := db.UpdateA2ATask(rec, 1); err != nil || v != 2 {
		t.Fatalf("UpdateA2ATask(prev 1) = (%d, %v), want (2, nil)", v, err)
	}
	if _, err := db.UpdateA2ATask(rec, 1); !errors.Is(err, store.ErrA2ATaskConflict) {
		t.Fatalf("stale update error = %v, want ErrA2ATaskConflict", err)
	}
	if v, err := db.UpdateA2ATask(rec, 0); err != nil || v != 3 {
		t.Fatalf("unversioned update = (%d, %v), want (3, nil)", v, err)
	}
	other := rec
	other.Owner = "someone-else"
	if _, err := db.UpdateA2ATask(other, 0); !errors.Is(err, store.ErrA2ATaskNotFound) {
		t.Fatalf("other owner's update error = %v, want ErrA2ATaskNotFound", err)
	}
	missing := rec
	missing.TaskID = "nope"
	if _, err := db.UpdateA2ATask(missing, 0); !errors.Is(err, store.ErrA2ATaskNotFound) {
		t.Fatalf("update of a missing task error = %v, want ErrA2ATaskNotFound", err)
	}
	if _, err := db.GetA2ATask("b", "nope"); !errors.Is(err, store.ErrA2ATaskNotFound) {
		t.Fatalf("GetA2ATask of a missing task error = %v, want ErrA2ATaskNotFound", err)
	}
	if _, err := db.GetA2ATask("other-binding", "t1"); !errors.Is(err, store.ErrA2ATaskNotFound) {
		t.Fatalf("GetA2ATask across bindings error = %v, want ErrA2ATaskNotFound", err)
	}
}
