package turnrouting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	gomsg "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func stage(t *testing.T, db *store.Store, kind string) gomsg.Envelope {
	t.Helper()
	env, err := db.StageTurnOutput(context.Background(), gomsg.Envelope{From: gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: "routed"}, Payload: []byte(`{"text":"answer"}`), ContentType: "application/json", Metadata: map[string]string{"session_id": "routed", "turn_id": uuid.NewString(), "kind": kind}})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func database(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(store.SessionRow{ID: "routed", LaunchID: "launch", State: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, &launch.Plan{Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}); err != nil {
		t.Fatal(err)
	}
	return db, path
}

type notifyingStore struct {
	*store.Store
	attached chan string
}

func (s *notifyingStore) AttachChannelMessage(ctx context.Context, req channels.ExistingMessage) (gomsg.Envelope, error) {
	env, err := s.Store.AttachChannelMessage(ctx, req)
	if err == nil {
		s.attached <- env.ID
	}
	return env, err
}

func receive(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(10 * time.Second):
		t.Fatal("router did not attach")
		return ""
	}
}

func TestRestartRecoversStageWithoutEvent(t *testing.T) {
	db, path := database(t)
	staged := stage(t, db, "final")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	attached := make(chan string, 2)
	router := New(reopened, events.NewBus(events.BusOptions{Persister: reopened}), channels.New(&notifyingStore{Store: reopened, attached: attached}, nil))
	if err := router.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if receive(t, attached) != staged.ID {
		t.Fatal("message copied")
	}
	router.Close()
	if router.Running() {
		t.Fatal("worker remains running after join")
	}
	history, err := reopened.ReadChannel(context.Background(), "ops", 0, 10)
	if err != nil || len(history) != 1 || history[0].ID != staged.ID {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestLiveSinkAndSessionPrincipal(t *testing.T) {
	db, _ := database(t)
	attached := make(chan string, 2)
	db.SetChannelAuthorization(func(ctx context.Context, operation, name string, p identity.Principal, from gomsg.Address) error {
		if operation != "publish" || name != "ops" || p.ID != "msg://session/local/routed" || p.Kind != "session" || p.CreatedBy != actor.URN() || from.URN() != p.ID {
			return errors.New("wrong publisher identity")
		}
		return nil
	})
	bus := events.NewBus(events.BusOptions{Persister: db})
	router := New(db, bus, channels.New(&notifyingStore{Store: db, attached: attached}, nil))
	if err := router.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	staged := stage(t, db, "final")
	body, _ := json.Marshal(events.TurnOutputEvent{SessionID: "routed", MessageID: staged.ID})
	if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: "routed", Kind: events.KindSessionTurnOutput, PayloadJSON: string(body)}); err != nil {
		t.Fatal(err)
	}
	if receive(t, attached) != staged.ID {
		t.Fatal("message copied")
	}
}

func TestPendingPagesAdvancePastFailuresAndRetry(t *testing.T) {
	db, _ := database(t)
	bad := stage(t, db, "approval")
	for range 130 {
		stage(t, db, "approval")
	}
	for range 130 {
		stage(t, db, "final")
	}
	router := New(db, nil, channels.New(db, nil))
	router.scan(context.Background())
	history, err := db.ReadChannel(context.Background(), "ops", 0, 1000)
	if err != nil || len(history) != 130 {
		t.Fatalf("pending page stopped: %d %v", len(history), err)
	}
	if _, err := db.StagedTurnOutput(context.Background(), bad.ID); err != nil {
		t.Fatal(err)
	}
	if len(router.retries) != 131 {
		t.Fatalf("retry records=%d", len(router.retries))
	}
	// A transient denial leaves the row staged and a later full sweep retries it.
	final := stage(t, db, "final")
	db.SetChannelAuthorization(func(context.Context, string, string, identity.Principal, gomsg.Address) error {
		return errors.New("temporarily denied")
	})
	router.scan(context.Background())
	if _, err := db.StagedTurnOutput(context.Background(), final.ID); err != nil {
		t.Fatal(err)
	}
	db.SetChannelAuthorization(nil)
	delete(router.retries, final.ID) // advance retry deadline deterministically
	router.scan(context.Background())
	if _, err := db.StagedTurnOutput(context.Background(), final.ID); !errors.Is(err, gomsg.ErrNotFound) {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestWorkerUsesLiveEventsAndPeriodicRecovery(t *testing.T) {
	for _, mode := range []string{"live", "periodic"} {
		t.Run(mode, func(t *testing.T) {
			db, _ := database(t)
			attached := make(chan string, 2)
			scanned := make(chan struct{}, 2)
			ticks := make(chan time.Time)
			bus := events.NewBus(events.BusOptions{Persister: db})
			router := New(db, bus, channels.New(&notifyingStore{Store: db, attached: attached}, nil))
			router.ticks = ticks
			router.afterScan = func() { scanned <- struct{}{} }
			if err := router.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer router.Close()
			select {
			case <-scanned:
			case <-time.After(10 * time.Second):
				t.Fatal("initial scan stuck")
			}
			staged := stage(t, db, "final")
			if mode == "live" {
				body, _ := json.Marshal(events.TurnOutputEvent{SessionID: "routed", MessageID: staged.ID})
				if err := bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: "routed", Kind: events.KindSessionTurnOutput, PayloadJSON: string(body)}); err != nil {
					t.Fatal(err)
				}
			} else {
				ticks <- time.Now()
			}
			if receive(t, attached) != staged.ID {
				t.Fatal("wrong message")
			}
		})
	}
}
func TestRetryBackoffAndExpiredQueueEviction(t *testing.T) {
	db, _ := database(t)
	env := stage(t, db, "final")
	calls := 0
	db.SetChannelAuthorization(func(context.Context, string, string, identity.Principal, gomsg.Address) error {
		calls++
		return channels.ErrForbidden
	})
	router := New(db, nil, channels.New(db, nil))
	now := time.Now()
	router.now = func() time.Time { return now }
	item := store.PendingTurnOutput{MessageID: env.ID, SessionID: "routed"}
	for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute} {
		before := calls
		router.attempt(context.Background(), item)
		if calls != before+1 || router.retries[env.ID].delay != delay {
			t.Fatalf("retry=%+v calls=%d", router.retries[env.ID], calls)
		}
		router.attempt(context.Background(), item)
		if calls != before+1 {
			t.Fatal("retried before deadline")
		}
		now = router.retries[env.ID].next
	}
	if _, err := db.DB().Exec(`UPDATE messages SET created_at=? WHERE id=?`, time.Now().Add(-store.RoutingStageRetention-time.Hour).UTC().Format(time.RFC3339Nano), env.ID); err != nil {
		t.Fatal(err)
	}
	router.scan(context.Background())
	if len(router.retries) != 0 {
		t.Fatal("expired row retains retry state")
	}
}

func TestRecoverySweepAttachesInStagingOrder(t *testing.T) {
	db, _ := database(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Minute)
	var want []string
	for i := 0; i < 20; i++ {
		env := stage(t, db, "final")
		id := fmt.Sprintf("backlog-%02d", 20-i)
		if _, err := db.DB().Exec(`UPDATE messages SET id=?,created_at=? WHERE id=?`, id, base.Add(time.Duration(i)*2*time.Millisecond).Format(time.RFC3339Nano), env.ID); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	router := New(db, nil, channels.New(db, nil))
	router.scan(ctx)
	history, err := db.ReadChannel(ctx, "ops", 0, 100)
	if err != nil || len(history) != len(want) {
		t.Fatalf("backlog recovery: %d %v", len(history), err)
	}
	for i, env := range history {
		if env.ID != want[i] {
			t.Fatalf("attachment %d: got %q want %q", i, env.ID, want[i])
		}
	}
}
