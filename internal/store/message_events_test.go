package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func drainMessages(t *testing.T, st *store.Store) []events.Event {
	t.Helper()
	bus := events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	for range 20 {
		n, err := st.DrainMessageEvents(context.Background(), bus)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	all, err := st.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	return all
}
func messageEventPayload(t *testing.T, ev events.Event) map[string]any {
	t.Helper()
	entry, ok := catalog.Lookup(ev.Kind)
	if !ok {
		t.Fatalf("unknown canonical kind %s", ev.Kind)
	}
	raw, err := catalog.PayloadSchema(entry.PayloadSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if err = resolved.Validate(payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ev.PayloadJSON, "BODY-CANARY") || strings.Contains(ev.PayloadJSON, "ERROR-CANARY") || strings.Contains(ev.PayloadJSON, "LEASE-CANARY") || strings.Contains(ev.PayloadJSON, "METADATA-CANARY") {
		t.Fatal("content or secret copied into canonical event")
	}
	for _, key := range []string{"body", "payload", "metadata", "error", "detail", "wake_text", "lease_token"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("forbidden field %s", key)
		}
	}
	return payload
}
func sendEventMessage(t *testing.T, st *store.Store) messaging.Envelope {
	t.Helper()
	env, err := st.MessagingStore().Send(context.Background(), messaging.Envelope{From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}, To: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "receiver"}, Kind: messaging.MsgKindNotice, ThreadID: "thread", InReplyTo: "parent", ContentType: "application/json", Payload: json.RawMessage(`{"body":"BODY-CANARY"}`), Metadata: map[string]string{"urgency": "high", "private": "METADATA-CANARY"}})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestMessageEventsLifecycleRestartAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env := sendEventMessage(t, st)
	mail := st.MessagingStore()
	if _, err = mail.Inbox(ctx, env.To, messaging.Filter{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = mail.MarkRead(ctx, env.ID, env.To); err != nil {
			t.Fatal(err)
		}
		if err = mail.Consume(ctx, env.ID, env.To); err != nil {
			t.Fatal(err)
		}
		if err = mail.Archive(ctx, env.ID, env.To); err != nil {
			t.Fatal(err)
		}
		if err = mail.Cancel(ctx, env.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.PurgeMessageBody(ctx, env.ID, env.From.URN()); err != nil {
		t.Fatal(err)
	}
	rows := drainMessages(t, st)
	counts := map[string]int{}
	for _, ev := range rows {
		payload := messageEventPayload(t, ev)
		counts[ev.Kind]++
		if payload["message_id"] != env.ID || payload["from"] != env.From.URN() || payload["to"].([]any)[0] != env.To.URN() {
			t.Fatal(payload)
		}
		if ev.EventID == "" || ev.Seq <= 0 || payload["body_ref"] != "/messages/"+env.ID {
			t.Fatal("missing durable identity/ref")
		}
	}
	for _, kind := range []string{"message.sent", "message.queued", "message.delivered", "message.read", "message.consumed", "message.archived", "message.canceled", "message.purged"} {
		if counts[kind] != 1 {
			t.Fatalf("%s=%d counts=%v", kind, counts[kind], counts)
		}
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	again := drainMessages(t, st)
	if len(again) != len(rows) {
		t.Fatal("restart replayed journal")
	}
	for i := range rows {
		if again[i].EventID != rows[i].EventID || again[i].Seq != rows[i].Seq {
			t.Fatal("durable IDs changed")
		}
	}
}

func TestMessageEventsFailedDeadLetterRedriveAndBinding(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	env := sendEventMessage(t, st)
	ds := st.DeliveryStore()
	id, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	claim, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(id), Holder: "holder", LeaseDuration: time.Minute, BindingGeneration: 7})
	if err != nil {
		t.Fatal(err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Delivery.ID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: 7}
	if _, _, err = ds.Nack(ctx, delivery.NackRequest{Lease: lease, Error: "ERROR-CANARY", Retryable: false}); err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		t.Fatal(err)
	}
	if _, err = ds.Redrive(ctx, delivery.RedriveRequest{DeliveryID: claim.Delivery.ID, AuthorizedBy: env.From.URN()}); err != nil {
		t.Fatal(err)
	}
	rows := drainMessages(t, st)
	counts := map[string]int{}
	lastTransition := float64(0)
	for _, ev := range rows {
		payload := messageEventPayload(t, ev)
		counts[ev.Kind]++
		tr := payload["transition_id"].(float64)
		if tr < lastTransition {
			t.Fatal("transition cursor reversed")
		}
		lastTransition = tr
		if ev.Kind == "message.failed" || ev.Kind == "message.dead_lettered" {
			d := payload["delivery"].(map[string]any)
			if d["attempt"] != float64(1) || d["binding_generation"] != float64(7) || d["attempt_id"] != string(claim.Attempt.ID) {
				t.Fatal(d)
			}
		}
	}
	if counts["message.failed"] != 1 || counts["message.dead_lettered"] != 1 || counts["message.redriven"] != 1 || counts["message.queued"] != 2 {
		t.Fatal(counts)
	}
}

func TestMessageEventsOutboxFailureDoesNotAdvanceCheckpoint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sendEventMessage(t, st)
	if _, err = st.DB().Exec(`CREATE TRIGGER fail_message_event BEFORE INSERT ON events WHEN NEW.kind LIKE 'message.%' BEGIN SELECT RAISE(ABORT,'event fault'); END`); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	if _, err = st.DrainMessageEvents(context.Background(), bus); err == nil {
		t.Fatal("expected event fault")
	}
	var checkpoint int
	if err = st.DB().QueryRow(`SELECT position FROM message_event_checkpoints WHERE source='delivery'`).Scan(&checkpoint); err != nil || checkpoint != 0 {
		t.Fatal("checkpoint survived rollback", err)
	}
	if _, err = st.DB().Exec(`DROP TRIGGER fail_message_event`); err != nil {
		t.Fatal(err)
	}
	rows := drainMessages(t, st)
	if len(rows) != 2 {
		t.Fatalf("recovery lost/duplicated observations: %d", len(rows))
	}
}

func TestMessageEventsGroupFanoutUsesCanonicalMailboxID(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	reg := registry.NewService(registry.NewStorage(st.DB()))
	reg.SetDeliveryStore(st.DeliveryStore())
	owner, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	group, err := reg.Register(ctx, registry.KindGroup, registry.Profile{DisplayName: "room", LastUpdatedBy: owner.URN})
	if err != nil {
		t.Fatal(err)
	}
	recipients := map[string]bool{}
	for _, name := range []string{"bob", "carol"} {
		p, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: name})
		if err != nil {
			t.Fatal(err)
		}
		recipients[p.URN] = true
		if _, err = reg.AddMember(ctx, group.URN, p.URN, owner.URN, registry.MemberRoleMember); err != nil {
			t.Fatal(err)
		}
	}
	gm, err := reg.SendToGroup(ctx, group.URN, owner.URN, "notice", "thread", "application/json", json.RawMessage(`{"body":"BODY-CANARY"}`))
	if err != nil {
		t.Fatal(err)
	}
	for recipient := range recipients {
		if err = reg.RemoveMember(ctx, group.URN, recipient, owner.URN); err != nil {
			t.Fatal(err)
		}
		break
	}
	rows := drainMessages(t, st)
	if len(rows) != 4 {
		t.Fatalf("fanout events=%d want two per frozen recipient", len(rows))
	}
	counts := map[string]int{}
	for _, ev := range rows {
		payload := messageEventPayload(t, ev)
		to := payload["to"].([]any)[0].(string)
		counts[to]++
		if payload["message_id"] != gm.ID || !recipients[to] {
			t.Fatalf("wrong canonical group identity: %v", payload)
		}
	}
	for recipient := range recipients {
		if counts[recipient] != 2 {
			t.Fatal(counts)
		}
	}
}
