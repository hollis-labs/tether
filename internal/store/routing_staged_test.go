package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/tether/internal/store"
)

func stageOutput(t *testing.T, db *store.Store) messaging.Envelope {
	t.Helper()
	env, err := db.StageTurnOutput(context.Background(), messaging.Envelope{
		From:     messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"},
		ThreadID: "s1", Payload: []byte(`{"text":"` + strings.Repeat("reply", 2000) + `"}`),
		ContentType: "application/json", Metadata: map[string]string{"turn_id": "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestStagedOutputHiddenFromMessagesAndDelivery(t *testing.T) {
	db := openRetentionDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ms := db.MessagingStore()
	sub, err := ms.Subscribe(ctx, messaging.Address{}, messaging.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	staged := stageOutput(t, db)
	select {
	case env := <-sub:
		t.Fatalf("staging notified subscriber: %+v", env)
	default:
	}
	if _, err := ms.Get(ctx, staged.ID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("Get exposed stage: %v", err)
	}
	if page, err := ms.List(ctx, staged.To, store.ListFilter{IncludeArchived: true}); err != nil || page.Total != 0 {
		t.Fatalf("List: %+v %v", page, err)
	}
	if rows, err := db.ListMessages(100); err != nil || len(rows) != 0 {
		t.Fatalf("global List: %+v %v", rows, err)
	}
	if rows, err := ms.Thread(ctx, staged.ThreadID, messaging.Filter{}); err != nil || len(rows) != 0 {
		t.Fatalf("Thread: %+v %v", rows, err)
	}
	if rows, err := ms.Inbox(ctx, staged.To, messaging.Filter{}); err != nil || len(rows) != 0 {
		t.Fatalf("Inbox: %+v %v", rows, err)
	}
	for _, mutate := range []func(context.Context, string, messaging.Address) error{ms.Consume, ms.MarkRead, ms.Archive, ms.Unarchive} {
		if err := mutate(ctx, staged.ID, staged.To); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("stage mutated: %v", err)
		}
	}
	if err := ms.Cancel(ctx, staged.ID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("Cancel: %v", err)
	}
	result, err := store.ImportLegacyMessagesIntoDelivery(ctx, db.DB(), db.DeliveryStore(), store.ImportHoldAmbiguousDelivered)
	if err != nil {
		t.Fatal(err)
	}
	_ = result
	ready, err := db.DeliveryStore().ListDeliveries(ctx, delivery.Filter{ReadyOnly: true})
	if err != nil || len(ready) != 0 {
		t.Fatalf("stage queued: %+v %v", ready, err)
	}
	if _, ok, err := db.DeliveryIDForMessage(ctx, staged.ID); err != nil || ok {
		t.Fatalf("stage delivery id: %v %v", ok, err)
	}
	// Even a recipient changed to the User inbox remains hidden by the flag.
	user := messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"}
	if _, err := db.DB().Exec(`UPDATE messages SET to_urn=? WHERE id=?`, user.URN(), staged.ID); err != nil {
		t.Fatal(err)
	}
	if page, err := ms.List(ctx, user, store.ListFilter{}); err != nil || page.Total != 0 {
		t.Fatalf("User inbox: %+v %v", page, err)
	}
	saved, err := db.StagedTurnOutput(ctx, staged.ID)
	if err != nil || string(saved.Payload) != string(staged.Payload) || saved.DeliveredAt != nil || saved.ConsumedAt != nil {
		t.Fatalf("internal stage: %+v %v", saved, err)
	}
}

func TestStagedOutputSurvivesRestartAndExpiresForRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	env := stageOutput(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	saved, err := db.StagedTurnOutput(ctx, env.ID)
	if err != nil || string(saved.Payload) != string(env.Payload) {
		t.Fatalf("restart stage: %+v %v", saved, err)
	}
	if _, err := db.PurgeMessageBody(ctx, env.ID, "msg://user/local/operator"); !errors.Is(err, store.ErrPendingObligation) {
		t.Fatalf("premature purge: %v", err)
	}
	old := time.Now().Add(-store.RoutingStageRetention - time.Hour)
	if _, err := db.DB().Exec(`UPDATE messages SET created_at=? WHERE id=?`, old.UTC().Format(time.RFC3339Nano), env.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := db.ListRetentionCandidates(ctx, time.Now())
	if err != nil || len(candidates) != 1 || !candidates[0].Eligible || candidates[0].HasDelivery {
		t.Fatalf("expired candidates: %+v %v", candidates, err)
	}
	if purged, err := db.PurgeMessageBody(ctx, env.ID, "msg://user/local/operator"); err != nil || !purged {
		t.Fatalf("expired purge: %v %v", purged, err)
	}
	if _, err := db.StagedTurnOutput(ctx, env.ID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("purged stage returned: %v", err)
	}
	if rows, err := db.ListMessages(10); err != nil || len(rows) != 0 {
		t.Fatalf("purged stage exposed: %+v %v", rows, err)
	}
}
