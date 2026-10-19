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
		if ev.EventID == "" || ev.Seq <= 0 || !strings.HasPrefix(payload["body_ref"].(string), "/messages/"+env.ID+"?as=") {
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
	for recipient := range recipients {
		if err := reg.MarkRead(ctx, group.URN, recipient, gm.GroupSeq); err != nil {
			continue
		} // removed member is no longer allowed
		if err := reg.MarkRead(ctx, group.URN, recipient, gm.GroupSeq); err != nil {
			t.Fatal(err)
		}
		all := drainMessages(t, st)
		if len(all) != 5 {
			t.Fatalf("group read was missing or replayed: %d", len(all))
		}
		payload := messageEventPayload(t, all[len(all)-1])
		if all[len(all)-1].Kind != "message.read" || payload["message_id"] != gm.ID || payload["to"].([]any)[0] != recipient {
			t.Fatal(payload)
		}
		return
	}
	t.Fatal("no remaining group member")
}

func TestMessageEventsPagingDeferredAndStreamControl(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// Public delivery-core enqueue may commit before the mailbox body. Keep its
	// source reference without allowing it to poison the rest of the stream.
	orphan, err := st.DeliveryStore().Enqueue(ctx, delivery.EnqueueRequest{From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "a"}, Recipients: []delivery.RecipientTarget{{Address: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "b"}}}, Kind: messaging.MsgKindNotice, Payload: []byte("BODY-CANARY")})
	if err != nil {
		t.Fatal(err)
	}
	for range 130 {
		sendEventMessage(t, st)
	}
	if _, err = st.DB().Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,created_at) VALUES ('control','stream.cursor','msg://agent/test/a','msg://agent/test/b',?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	rows := drainMessages(t, st)
	if len(rows) != 260 {
		t.Fatalf("paging lost events or emitted stream control: %d", len(rows))
	}
	var deferred int
	if err = st.DB().QueryRow(`SELECT count(*) FROM message_event_deferred`).Scan(&deferred); err != nil || deferred != 1 {
		t.Fatal("missing durable deferred reference", deferred, err)
	}
	// Repair after the source checkpoint passed: the original durable IDs remain.
	if _, err = st.DB().Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,payload,created_at,delivery_id) VALUES (?,'notice',?,?,?, ?,?)`, orphan.Message.ID, orphan.Message.From.URN(), orphan.Deliveries[0].Recipient.URN(), "BODY-CANARY", orphan.Message.CreatedAt.Format(time.RFC3339Nano), orphan.Deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	rows = drainMessages(t, st)
	if len(rows) != 262 {
		t.Fatalf("deferred recovery count=%d", len(rows))
	}
	for _, ev := range rows {
		messageEventPayload(t, ev)
	}
	if err = st.DB().QueryRow(`SELECT count(*) FROM message_event_deferred`).Scan(&deferred); err != nil || deferred != 0 {
		t.Fatal("deferred observation not settled", deferred, err)
	}
}

func TestMessageEventsTeamRunStampUsesPersistedRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err = st.DB().Exec(`INSERT INTO team_runs(run_id,session_group_id,team_id,team_version,payload,created_at) VALUES ('run-1','group-1','team',1,'{}',?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	known := sendEventMessage(t, st)
	// Retry-safe normal sends accept metadata, but only a persisted run can be
	// stamped as a scope dimension; no other metadata enters the event.
	if _, err = st.DB().Exec(`UPDATE messages SET metadata='{"team_run":"run-1"}' WHERE id=?`, known.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.MessagingStore().MarkRead(ctx, known.ID, known.To); err != nil {
		t.Fatal(err)
	}
	unknown := sendEventMessage(t, st)
	if _, err = st.DB().Exec(`UPDATE messages SET metadata='{"team_run":"METADATA-CANARY"}' WHERE id=?`, unknown.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.MessagingStore().MarkRead(ctx, unknown.ID, unknown.To); err != nil {
		t.Fatal(err)
	}
	rows := drainMessages(t, st)
	seen := 0
	for _, ev := range rows {
		payload := messageEventPayload(t, ev)
		if ev.Kind != "message.read" {
			continue
		}
		seen++
		if payload["message_id"] == known.ID {
			if ev.Envelope.Dims.TeamRunID != "run-1" {
				t.Fatal("known run stamp lost")
			}
		} else if ev.Envelope.Dims.TeamRunID != "" {
			t.Fatal("unknown metadata became run scope")
		}
	}
	if seen != 2 {
		t.Fatal("read events missing")
	}
}

func TestMessageEventsDeferredRetryRotatesPastPoisonAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "recipient"}
	var late delivery.EnqueueResult
	for range 130 {
		late, err = st.DeliveryStore().Enqueue(ctx, delivery.EnqueueRequest{From: from, Recipients: []delivery.RecipientTarget{{Address: to}}, Kind: messaging.MsgKindNotice})
		if err != nil {
			t.Fatal(err)
		}
	}
	bus := events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	for range 2 {
		if _, err = st.DrainMessageEvents(ctx, bus); err != nil {
			t.Fatal(err)
		}
	}
	// 129 permanent holes precede this repaired mailbox. Persisted rotation must
	// reach it even though the oldest retry page never becomes ready.
	if _, err = st.DB().Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,created_at,delivery_id) VALUES (?,'notice',?,?,?,?)`, late.Message.ID, from.URN(), to.URN(), late.Message.CreatedAt.Format(time.RFC3339Nano), late.Deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bus = events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	for range 3 {
		if _, err = st.DrainMessageEvents(ctx, bus); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.EventsSince(0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("late ready observation starved: %d events %v", len(rows), err)
	}
	for _, ev := range rows {
		payload := messageEventPayload(t, ev)
		if payload["message_id"] != string(late.Message.ID) {
			t.Fatal(payload)
		}
	}
}
