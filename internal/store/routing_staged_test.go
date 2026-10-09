package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/messaging/delivery"
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

func TestStageOutputRejectsForeignSessionEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		from   messaging.Address
		thread string
		meta   map[string]string
	}{
		{"non-session", messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "s1"}, "s1", nil},
		{"foreign authority", messaging.Address{Kind: messaging.KindSession, Authority: "remote", ID: "s1"}, "s1", nil},
		{"foreign thread", messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, "other", nil},
		{"foreign metadata", messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, "s1", map[string]string{"session_id": "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openRetentionDB(t)
			if _, err := db.StageTurnOutput(context.Background(), messaging.Envelope{From: tc.from, ThreadID: tc.thread, Metadata: tc.meta, Payload: []byte(`{"text":"answer"}`)}); err == nil {
				t.Fatal("staged foreign envelope")
			}
		})
	}
}
func TestStageRetentionProtectsTwentyNineDaysAndExpiresAfterThirty(t *testing.T) {
	db := openRetentionDB(t)
	for _, days := range []int{29, 31} {
		env := stageOutput(t, db)
		if _, err := db.DB().Exec(`UPDATE messages SET created_at=? WHERE id=?`, time.Now().Add(-time.Duration(days)*24*time.Hour).UTC().Format(time.RFC3339Nano), env.ID); err != nil {
			t.Fatal(err)
		}
		purged, err := db.PurgeMessageBody(context.Background(), env.ID, "msg://user/local/operator")
		if days == 29 {
			if !errors.Is(err, store.ErrPendingObligation) || purged {
				t.Fatalf("29-day stage lost: %v %v", purged, err)
			}
		} else if err != nil || !purged {
			t.Fatalf("31-day stage retained: %v %v", purged, err)
		}
	}
}

func TestStageTurnOutputIdempotentAcrossCallsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idempotent.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := stageOutput(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	again := stageOutput(t, db)
	if again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("restaged output: %+v %+v", first, again)
	}
	// A later retry cannot replace the body already attached by the router.
	if _, err := db.DB().Exec(`UPDATE messages SET channel='ops', routing_staged=0 WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := db.StageTurnOutput(context.Background(), messaging.Envelope{From: first.From, Payload: first.Payload, ContentType: first.ContentType, Metadata: first.Metadata})
	if err != nil || retry.ID != first.ID || retry.Channel != "ops" || string(retry.Payload) != string(first.Payload) {
		t.Fatalf("retry replaced attached output: %+v %v", retry, err)
	}
	for _, tc := range []struct{ session, turn, kind string }{{"s2", "t1", ""}, {"s1", "t2", ""}, {"s1", "t1", "failure"}} {
		env, err := db.StageTurnOutput(context.Background(), messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: tc.session}, Payload: []byte("other"), Metadata: map[string]string{"turn_id": tc.turn, "kind": tc.kind}})
		if err != nil || env.ID == first.ID {
			t.Fatalf("distinct output collided: %+v %v", env, err)
		}
	}
}

func TestStageTurnOutputEmptyIdentityDoesNotCollapse(t *testing.T) {
	db := openRetentionDB(t)
	input := messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, Payload: []byte(`{"text":"answer"}`), ContentType: "application/json", Metadata: map[string]string{"kind": "final"}}
	first, err := db.StageTurnOutput(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.StageTurnOutput(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("unidentified distinct outputs collapsed")
	}
	for _, id := range []string{first.ID, second.ID} {
		if _, err := db.StagedTurnOutput(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}
func TestStageTurnOutputReusedIdentityPreservesBothBodiesAndRetries(t *testing.T) {
	db := openRetentionDB(t)
	first := stageOutput(t, db)
	input := messaging.Envelope{From: first.From, Payload: []byte(`{"text":"different answer"}`), ContentType: first.ContentType, Metadata: first.Metadata}
	second, err := db.StageTurnOutput(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || string(second.Payload) != string(input.Payload) {
		t.Fatalf("reused identity lost new body: %+v", second)
	}
	retry, err := db.StageTurnOutput(context.Background(), input)
	if err != nil || retry.ID != second.ID {
		t.Fatalf("alternate body retry duplicated: %+v %v", retry, err)
	}
	original, err := db.StagedTurnOutput(context.Background(), first.ID)
	if err != nil || string(original.Payload) != string(first.Payload) {
		t.Fatal("original overwritten", err)
	}
	if _, err := db.DB().Exec(`UPDATE messages SET channel='ops', routing_staged=0 WHERE id=?`, second.ID); err != nil {
		t.Fatal(err)
	}
	retry, err = db.StageTurnOutput(context.Background(), input)
	if err != nil || retry.ID != second.ID || retry.Channel != "ops" {
		t.Fatalf("attached alternate retry lost existing ID: %+v %v", retry, err)
	}
}

func TestStageTurnOutputContentTypeConflictPreservesBothRepresentations(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	first := stageOutput(t, db)
	input := messaging.Envelope{From: first.From, Payload: first.Payload, Metadata: first.Metadata, ContentType: "text/plain"}
	second, err := db.StageTurnOutput(ctx, input)
	if err != nil || second.ID == first.ID || second.ContentType != input.ContentType || string(second.Payload) != string(first.Payload) {
		t.Fatalf("content-type-only conflict lost: id=%q type=%q err=%v", second.ID, second.ContentType, err)
	}
	retry, err := db.StageTurnOutput(ctx, input)
	if err != nil || retry.ID != second.ID || retry.ContentType != input.ContentType {
		t.Fatalf("alternate representation retry lost: id=%q type=%q err=%v", retry.ID, retry.ContentType, err)
	}
	original, err := db.StagedTurnOutput(ctx, first.ID)
	if err != nil || original.ContentType != first.ContentType {
		t.Fatal("original representation overwritten", err)
	}
}

func TestStageTurnOutputKeepsNativeSourceAttributionDistinct(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	var previous messaging.Envelope
	for _, source := range []string{"completion-one", "completion-two"} {
		outputID := store.TurnOutputID("s1", "t1", "approval", source, "same question")
		input := messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"},
			Payload: []byte(`{"text":"same question"}`), ContentType: "application/json",
			Metadata: map[string]string{"turn_id": "t1", "kind": "approval", "output_id": outputID, "provider_result_id": source}}
		first, err := db.StageTurnOutput(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID == previous.ID || first.Metadata["provider_result_id"] != source || first.Metadata["output_id"] != outputID {
			t.Fatal("native source attribution collapsed")
		}
		retry, err := db.StageTurnOutput(ctx, input)
		if err != nil || retry.ID != first.ID || !retry.CreatedAt.Equal(first.CreatedAt) {
			t.Fatal("native source retry duplicated", err)
		}
		input.Payload = []byte(`{"text":"conflicting question"}`)
		if _, err := db.StageTurnOutput(ctx, input); err == nil {
			t.Fatal("accepted a conflicting body under one output identity")
		}
		previous = first
	}
}
