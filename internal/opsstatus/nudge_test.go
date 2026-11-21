package opsstatus

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

type fakeTasks struct {
	status, blocked map[string]string
	comments        map[string]bool // task+"/"+kind
	reads           int
}

func (f *fakeTasks) Task(_ context.Context, id string) (string, string, error) {
	f.reads++
	return f.status[id], f.blocked[id], nil
}
func (f *fakeTasks) HasStatusComment(_ context.Context, id, kind string) (bool, error) {
	return f.comments[id+"/"+kind], nil
}

type fakeNudgeStore struct {
	rows   []store.SessionRow
	nudged map[string]bool
}

func (f *fakeNudgeStore) ListSessions(store.ListSessionsOptions) ([]store.SessionRow, error) {
	return f.rows, nil
}
func (f *fakeNudgeStore) OpsStatusNudged(_ context.Context, s, t, k string) (bool, error) {
	return f.nudged[s+t+k], nil
}
func (f *fakeNudgeStore) MarkOpsStatusNudged(_ context.Context, s, t, k string) error {
	f.nudged[s+t+k] = true
	return nil
}

func row(id, task string) store.SessionRow {
	return store.SessionRow{ID: id, TaskID: sql.NullString{String: task, Valid: task != ""}}
}

func TestNudgerRemindsOnce(t *testing.T) {
	st := &fakeNudgeStore{nudged: map[string]bool{}, rows: []store.SessionRow{
		row("s-done", "CW-done"), row("s-posted", "CW-posted"), row("s-blocked", "CW-blocked"),
		row("s-reasoned", "CW-reasoned"), row("s-doing", "CW-doing"), row("s-none", ""), row("s-done-2", "CW-done"),
	}}
	tasks := &fakeTasks{
		status:   map[string]string{"CW-done": "done", "CW-posted": "done", "CW-blocked": "blocked", "CW-reasoned": "blocked", "CW-doing": "doing"},
		blocked:  map[string]string{"CW-reasoned": "waiting on review"},
		comments: map[string]bool{"CW-posted/work_done": true},
	}
	sent := map[string]string{}
	n := NewNudger(st, tasks, func(_ context.Context, id, text string) error { sent[id] = text; return nil }, 0)
	n.Tick(context.Background())
	if len(sent) != 3 || !strings.Contains(sent["s-done"], "work_done") || !strings.Contains(sent["s-blocked"], "blocked") || sent["s-done-2"] == "" {
		t.Fatalf("sent = %v", sent)
	}
	if tasks.reads != 5 {
		t.Fatalf("task reads = %d, want one per distinct task", tasks.reads)
	}
	if !st.nudged["s-postedCW-postedwork_done"] {
		t.Fatal("already-posted task not recorded as settled")
	}
	sent = map[string]string{}
	n.Tick(context.Background())
	if len(sent) != 0 {
		t.Fatalf("second tick re-sent: %v", sent)
	}
}
