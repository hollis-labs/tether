package app

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// idemHarness is a Service over a real state DB with the stub API runtime,
// enough to create sessions without a catalog.
func idemHarness(t *testing.T) (*Service, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Service{Store: st, factories: map[string]RuntimeFactory{"stub": stub.New}}, dbPath
}

func idemPlan(t *testing.T) func() (*launch.Plan, error) {
	t.Helper()
	root := t.TempDir()
	return func() (*launch.Plan, error) {
		return &launch.Plan{
			LaunchID: "demo", ProjectID: "project", LogicalAgentID: "agent",
			ProviderID: "stub", WriteHome: root, WorkspaceMode: "shared", RepoRoot: t.TempDir(),
		}, nil
	}
}

func countSessions(t *testing.T, svc *Service) int {
	t.Helper()
	rows, err := svc.Store.ListSessions(store.ListSessionsOptions{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// The lost-response case: the same key and request return the session the
// first request created, once, with Replayed set.
func TestCreateKeyed_SameKeyAndRequestReplays(t *testing.T) {
	svc, _ := idemHarness(t)
	build := idemPlan(t)
	first, err := svc.createKeyed("test/k1", "digest-a", build)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed {
		t.Fatal("first create reported Replayed")
	}
	second, err := svc.createKeyed("test/k1", "digest-a", build)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.SessionID != first.SessionID {
		t.Fatalf("second = %+v; want a replay of %s", second, first.SessionID)
	}
	if n := countSessions(t, svc); n != 1 {
		t.Fatalf("sessions = %d; want 1", n)
	}
}

func TestCreateKeyed_DifferentRequestConflicts(t *testing.T) {
	svc, _ := idemHarness(t)
	build := idemPlan(t)
	if _, err := svc.createKeyed("test/k1", "digest-a", build); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.createKeyed("test/k1", "digest-b", build); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("err = %v; want ErrIdempotencyConflict", err)
	}
	if n := countSessions(t, svc); n != 1 {
		t.Fatalf("sessions = %d; want 1", n)
	}
}

// A key claimed by a create conflicts with a resume, and vice versa.
func TestReplayIfKeyed_OperationMismatchConflicts(t *testing.T) {
	svc, _ := idemHarness(t)
	if _, err := svc.createKeyed("test/k1", resumeRequestDigest("agent"), idemPlan(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.replayIfKeyed("test/k1", store.IdempotencyOpResume, resumeRequestDigest("agent")); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("err = %v; want ErrIdempotencyConflict for a resume on a create key", err)
	}
}

// A create that fails before its row commits reserves nothing, so the retry
// proceeds as a first request.
func TestCreateKeyed_FailedCreateReservesNothing(t *testing.T) {
	svc, _ := idemHarness(t)
	failing := func() (*launch.Plan, error) {
		p, _ := idemPlan(t)()
		p.ProviderID = "no-such-provider"
		return p, nil
	}
	if _, err := svc.createKeyed("test/k1", "digest-a", failing); err == nil {
		t.Fatal("create with an unknown provider succeeded")
	}
	if rec, err := svc.Store.GetSessionIdempotency("test/k1"); err != nil || rec != nil {
		t.Fatalf("key after failed create = %+v, %v; want none", rec, err)
	}
	if l, err := svc.createKeyed("test/k1", "digest-a", idemPlan(t)); err != nil || l.Replayed {
		t.Fatalf("retry = %+v, %v; want a fresh create", l, err)
	}
}

// Concurrent requests for one key create exactly one session.
func TestCreateKeyed_ConcurrentSameKeyCreatesOnce(t *testing.T) {
	svc, _ := idemHarness(t)
	build := idemPlan(t)
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := svc.createKeyed("test/concurrent", "digest-a", build)
			if err != nil {
				t.Errorf("create %d: %v", i, err)
				return
			}
			ids[i] = l.SessionID
		}(i)
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("session ids differ: %v", ids)
		}
	}
	if got := countSessions(t, svc); got != 1 {
		t.Fatalf("sessions = %d; want 1", got)
	}
}

// A replayed session is returned as it now stands: a failed session stays
// failed and is never replaced.
func TestCreateKeyed_ReplaysAFailedSessionUnchanged(t *testing.T) {
	svc, _ := idemHarness(t)
	first, err := svc.createKeyed("test/k1", "digest-a", idemPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	exit := 1
	if err := svc.Store.UpdateSessionState(first.SessionID, string(session.StateFailed), 0, &exit); err != nil {
		t.Fatal(err)
	}
	again, err := svc.createKeyed("test/k1", "digest-a", idemPlan(t))
	if err != nil || !again.Replayed || again.SessionID != first.SessionID {
		t.Fatalf("replay = %+v, %v; want the failed session back", again, err)
	}
	row, err := svc.Store.GetSession(first.SessionID)
	if err != nil || row.State != string(session.StateFailed) {
		t.Fatalf("state = %v, %v; want failed", row, err)
	}
	if n := countSessions(t, svc); n != 1 {
		t.Fatalf("sessions = %d; want 1", n)
	}
}

// Launching a keyed session that is already past created replays it; an
// unkeyed one keeps the 409-backed ErrSessionNotCreated.
func TestLaunchSession_KeyedIsIdempotentUnkeyedIsNot(t *testing.T) {
	svc, _ := idemHarness(t)
	keyed, err := svc.createKeyed("test/k1", "digest-a", idemPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := idemPlan(t)()
	unkeyed, err := svc.createSessionFromPlan(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{keyed.SessionID, unkeyed.SessionID} {
		if err := svc.Store.UpdateSessionState(id, string(session.StateRunning), 42, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.LaunchSession(keyed.SessionID)
	if err != nil || !got.Replayed || got.SessionID != keyed.SessionID {
		t.Fatalf("keyed relaunch = %+v, %v; want a replay", got, err)
	}
	if _, err := svc.LaunchSession(unkeyed.SessionID); !errors.Is(err, session.ErrNotCreated) {
		t.Fatalf("unkeyed relaunch err = %v; want ErrNotCreated", err)
	}
}

// The case the digest rule exists for: a resume succeeds, its response is
// lost, the new session writes its own checkpoint, and the caller retries
// with the same key. The retry replays the original session rather than
// conflicting over the newer checkpoint the resolver would now pick.
func TestResumeLogicalAgent_RetryAfterNewCheckpointReplays(t *testing.T) {
	svc, _ := idemHarness(t)
	plan, _ := idemPlan(t)()
	// The session the first resume created, bound to its key as a resume.
	first, err := svc.createSessionFromPlan(plan, &store.SessionIdempotency{
		Key: "test/resume", Operation: store.IdempotencyOpResume, RequestDigest: resumeRequestDigest("agent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The resumed session then checkpoints, making it the newest parent.
	if err := svc.Store.CreateCheckpoint(checkpoint.Checkpoint{
		ID: "ck-new", LogicalAgentID: "agent", Status: "in_progress", Summary: "later work",
		SourceSessionID: first.SessionID, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ResumeLogicalAgent("agent", api.ResumeOptions{IdempotencyKey: "test/resume"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !res.Replayed || res.SessionID != first.SessionID {
		t.Fatalf("retry = %+v; want a replay of %s", res, first.SessionID)
	}
	if n := countSessions(t, svc); n != 1 {
		t.Fatalf("sessions = %d; want 1", n)
	}
}

// Keys live in the state DB, so a daemon restart keeps them.
func TestIdempotencyKey_SurvivesStoreReopen(t *testing.T) {
	svc, dbPath := idemHarness(t)
	first, err := svc.createKeyed("test/k1", "digest-a", idemPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Store.Close()
	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	svc2 := &Service{Store: reopened, factories: svc.factories}
	again, err := svc2.createKeyed("test/k1", "digest-a", idemPlan(t))
	if err != nil || !again.Replayed || again.SessionID != first.SessionID {
		t.Fatalf("after reopen = %+v, %v; want a replay of %s", again, err, first.SessionID)
	}
}

// The create digest covers every request field and nothing else.
func TestCreateRequestDigest(t *testing.T) {
	base := CreateSessionInput{LaunchID: "demo", BootPromptAppend: "x", IdempotencyKey: "k1"}
	if createRequestDigest(base) != createRequestDigest(CreateSessionInput{LaunchID: "demo", BootPromptAppend: "x", IdempotencyKey: "other"}) {
		t.Error("the key itself changed the digest")
	}
	for name, changed := range map[string]CreateSessionInput{
		"launch":        {LaunchID: "other", BootPromptAppend: "x"},
		"prompt_append": {LaunchID: "demo", BootPromptAppend: "y"},
		"agent_inline":  {LaunchID: "demo", BootPromptAppend: "x", AgentInline: "{}"},
		"injection":     {LaunchID: "demo", BootPromptAppend: "x", Injection: "{}"},
	} {
		if createRequestDigest(changed) == createRequestDigest(base) {
			t.Errorf("changing %s left the digest unchanged", name)
		}
	}
}
