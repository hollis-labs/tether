package opsstatus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func intp(v int) *int { return &v }

func TestClassify(t *testing.T) {
	cases := []struct {
		to, reason string
		exit       *int
		kind       string
		ok         bool
	}{
		{"running", "", nil, KindSessionStarted, true},
		{"completed", "", intp(0), KindSessionEnded, true},
		{"killed", "stop", nil, KindSessionEnded, true},
		{"killed", "team_stop", nil, KindSessionEnded, true},
		{"killed", "hub_kill:unknown_to_hub", nil, KindSessionLost, true},
		{"killed", "daemon-shutdown", nil, KindSessionLost, true},
		{"failed", "worker_exit", intp(0), KindSessionEnded, true},
		{"failed", "worker_exit", intp(137), KindSessionLost, true},
		{"failed", "swept at daemon start", intp(-1), KindSessionLost, true},
		{"failed", "never_started", nil, KindSessionLost, true},
		{"failed", "shim_recovery", intp(0), KindSessionLost, true},
		{"detached", "", nil, "", false},
		{"launching", "", nil, "", false},
	}
	for _, c := range cases {
		kind, _, ok := Classify(c.to, c.reason, c.exit)
		if kind != c.kind || ok != c.ok {
			t.Errorf("Classify(%q,%q) = %q,%v; want %q,%v", c.to, c.reason, kind, ok, c.kind, c.ok)
		}
	}
}

type fakeStore struct {
	rows map[string]*store.SessionRow
}

func (f *fakeStore) GetSession(id string) (*store.SessionRow, error) {
	if r, ok := f.rows[id]; ok {
		return r, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakeStore) MaxEventSeqContext(context.Context) (int64, error) { return 0, nil }
func (f *fakeStore) EventConsumerCursor(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}
func (f *fakeStore) AdvanceEventConsumerCursor(context.Context, string, int64) error { return nil }

func stateEvent(seq int64, session, to, reason string, exit *int) events.Event {
	raw, _ := json.Marshal(statePayload{From: "running", To: to, Reason: reason, ExitCode: exit})
	return events.Event{Seq: seq, At: time.Now(), SessionID: session, Kind: events.KindSessionStateChanged, PayloadJSON: string(raw)}
}

func TestPostForNeedsTaskOrWorkstream(t *testing.T) {
	fs := &fakeStore{rows: map[string]*store.SessionRow{
		"adhoc-session":  {ID: "adhoc-session", LogicalAgentID: "tether"},
		"tasked-session": {ID: "tasked-session", LogicalAgentID: "tether", TaskID: sql.NullString{String: "CW-1", Valid: true}, RunID: sql.NullString{String: "run-1", Valid: true}},
		"ws-session":     {ID: "ws-session", LogicalAgentID: "nanite", WorkerID: "worker-2", WorkstreamID: sql.NullString{String: "workstream:01ABC", Valid: true}},
	}}
	e := New(fs, nil, nil, Options{Host: "agent-os"})
	if _, ok := e.postFor(stateEvent(1, "adhoc-session", "running", "", nil)); ok {
		t.Fatal("ad-hoc session posted")
	}
	p, ok := e.postFor(stateEvent(2, "tasked-session", "running", "", nil))
	if !ok || p.Kind != KindSessionStarted || p.State != "started" || p.Task != "CW-1" || p.RunID != "run-1" {
		t.Fatalf("tasked start post = %+v, %v", p, ok)
	}
	if p.IdempotencyKey != "tether:tasked-session:session_started" || !strings.Contains(p.Summary, "on agent-os") {
		t.Fatalf("key/summary = %q / %q", p.IdempotencyKey, p.Summary)
	}
	p, ok = e.postFor(stateEvent(3, "ws-session", "killed", "hub_kill:hub_state_killed", nil))
	if !ok || p.Kind != KindSessionLost || p.WorkstreamID != "workstream:01ABC" || p.Body == "" {
		t.Fatalf("ws lost post = %+v, %v", p, ok)
	}
	if _, ok := e.postFor(stateEvent(4, "gone", "running", "", nil)); ok {
		t.Fatal("missing session posted")
	}
}

type recordingPoster struct {
	mu    sync.Mutex
	fails int // retryable failures before success
	err   error
	posts []Post
	tries int
}

func (r *recordingPoster) Post(_ context.Context, p Post) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tries++
	if r.err != nil {
		return r.err
	}
	if r.fails > 0 {
		r.fails--
		return &RetryableError{errors.New("ops: forbidden")}
	}
	r.posts = append(r.posts, p)
	return nil
}

func (r *recordingPoster) snapshot() ([]Post, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Post(nil), r.posts...), r.tries
}

func TestDeliverHoldsRetryableAndSkipsRefusalAndAge(t *testing.T) {
	ctx := context.Background()
	opts := Options{BackoffFloor: time.Millisecond, BackoffCeiling: time.Millisecond}

	held := &recordingPoster{fails: 3}
	New(&fakeStore{}, nil, held, opts).deliver(ctx, events.Event{Seq: 1, At: time.Now()}, Post{Kind: KindSessionStarted})
	if posts, tries := held.snapshot(); len(posts) != 1 || tries != 4 {
		t.Fatalf("held: posts=%d tries=%d", len(posts), tries)
	}

	refused := &recordingPoster{err: errors.New("ops: validation: bad")}
	New(&fakeStore{}, nil, refused, opts).deliver(ctx, events.Event{Seq: 2, At: time.Now()}, Post{})
	if _, tries := refused.snapshot(); tries != 1 {
		t.Fatalf("refusal retried: tries=%d", tries)
	}

	stale := &recordingPoster{fails: 1000}
	old := opts
	old.MaxAge = time.Hour
	New(&fakeStore{}, nil, stale, old).deliver(ctx, events.Event{Seq: 3, At: time.Now().Add(-2 * time.Hour)}, Post{})
	if _, tries := stale.snapshot(); tries != 1 {
		t.Fatalf("aged-out event retried: tries=%d", tries)
	}
}

func TestClientParsesOpsResponses(t *testing.T) {
	var gotAuth, gotActor, gotTask string
	var gotArgs map[string]any
	reply := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotActor, gotTask = r.Header.Get("Authorization"), r.Header.Get(headerActor), r.Header.Get(headerTask)
		var body struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotArgs = body.Params.Arguments
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{URL: srv.URL, TokenFile: tok, Actor: "msg://service/tether/test"}
	p := Post{State: "started", Kind: KindSessionStarted, Summary: "s", Task: "CW-1", Session: "abc", IdempotencyKey: "tether:abc:session_started", RunID: "run"}
	if err := c.Post(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || gotActor != "msg://service/tether/test" || gotTask != "CW-1" {
		t.Fatalf("headers: %q %q %q", gotAuth, gotActor, gotTask)
	}
	if gotArgs["kind"] != KindSessionStarted || gotArgs["idempotency_key"] != "tether:abc:session_started" || gotArgs["RunID"] != nil {
		t.Fatalf("args: %v", gotArgs)
	}

	var retry *RetryableError
	reply = `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"structuredContent":{"error":{"code":"forbidden","message":"operation outside configured authority"}}}}`
	if err := c.Post(context.Background(), p); !errors.As(err, &retry) {
		t.Fatalf("forbidden should hold: %v", err)
	}
	reply = `{"jsonrpc":"2.0","id":3,"result":{"isError":true,"structuredContent":{"error":{"code":"validation","message":"bad"}}}}`
	if err := c.Post(context.Background(), p); err == nil || errors.As(err, &retry) {
		t.Fatalf("validation should be a refusal: %v", err)
	}
	reply = "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":4,\"result\":{\"content\":[]}}\n"
	if err := c.Post(context.Background(), p); err != nil {
		t.Fatalf("sse result: %v", err)
	}
}

func TestEmitterFollowsAndResumesFromCursor(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	row := store.SessionRow{ID: "sess-1", ProjectID: "p", LogicalAgentID: "tether-lead", ProviderID: "claude", State: "created", CreatedAt: now, UpdatedAt: now,
		TaskID: sql.NullString{String: "CW-20261121-0154", Valid: true}}
	if err := db.CreateSession(row, &launch.Plan{TaskID: "CW-20261121-0154"}); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	publish := func(to, reason string, exit *int) {
		raw, _ := json.Marshal(statePayload{From: "running", To: to, Reason: reason, ExitCode: exit})
		if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "sess-1", Kind: events.KindSessionStateChanged, PayloadJSON: string(raw), At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// Before the emitter exists: never posted (it starts at the head).
	publish("running", "", nil)

	poster := &recordingPoster{}
	e := New(db, bus, poster, Options{Host: "agent-os"})
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	publish("running", "", nil)
	waitPosts(t, poster, 1)
	e.Close()

	// While stopped, the session ends; a restart resumes from the cursor.
	publish("completed", "", intp(0))
	e2 := New(db, bus, poster, Options{Host: "agent-os"})
	if err := e2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	posts := waitPosts(t, poster, 2)
	if posts[0].Kind != KindSessionStarted || posts[1].Kind != KindSessionEnded || posts[1].Task != "CW-20261121-0154" {
		t.Fatalf("posts = %+v", posts)
	}
}

func waitPosts(t *testing.T, r *recordingPoster, n int) []Post {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if posts, _ := r.snapshot(); len(posts) >= n {
			if len(posts) > n {
				t.Fatalf("got %d posts, want %d: %+v", len(posts), n, posts)
			}
			return posts
		}
		time.Sleep(10 * time.Millisecond)
	}
	posts, _ := r.snapshot()
	t.Fatalf("got %d posts, want %d", len(posts), n)
	return nil
}
