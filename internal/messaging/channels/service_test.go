package channels_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

const reader = "msg://user/local/observer"

func open(t *testing.T) (*store.Store, *channels.Service) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, channels.New(db, nil)
}

func publication(t *testing.T, name string) gomsg.Envelope {
	t.Helper()
	to, err := channels.ChannelAddress(name)
	if err != nil {
		t.Fatal(err)
	}
	return gomsg.Envelope{Kind: gomsg.MsgKindNotice, From: gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: "sender"}, To: to, Payload: json.RawMessage(`{"text":"hello"}`)}
}

func receive(t *testing.T, stream <-chan channels.Event) channels.Message {
	t.Helper()
	select {
	case event, ok := <-stream:
		if !ok || event.Err != nil {
			t.Fatalf("stream closed/error: %+v", event)
		}
		return event.Message
	case <-time.After(5 * time.Second):
		t.Fatal("channel delivery timed out")
	}
	return channels.Message{}
}

func TestChannelNamesAndAddresses(t *testing.T) {
	for _, name := range []string{"ops", "Ops.1_team-x", "a"} {
		address, err := channels.ChannelAddress(name)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := gomsg.ParseURN(address.URN())
		if err != nil || decoded != address {
			t.Fatalf("address round trip: %v %v", decoded, err)
		}
		got, ok := channels.AddressName(decoded)
		if !ok || got != name {
			t.Fatalf("name = %q, %v", got, ok)
		}
	}
	for _, name := range []string{"", ".", "..", "two words", "ops/new", "a?x", "é", fmt.Sprintf("%065d", 1)} {
		if channels.ValidateName(name) == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestChannelsDurableAndPrivateMailboxLabelsStayPrivate(t *testing.T) {
	db, svc := open(t)
	ctx := context.Background()
	env := publication(t, "ops")
	first, err := db.MessagingStore().Send(ctx, env) // Internal producers share normalization.
	if err != nil {
		t.Fatal(err)
	}
	if first.Channel != "ops" {
		t.Fatalf("channel = %q", first.Channel)
	}
	private := publication(t, "secret")
	private.To = gomsg.Address{Kind: gomsg.KindAgent, Authority: "local", ID: "private"}
	private.Channel = "secret"
	if _, err := db.MessagingStore().Send(ctx, private); err != nil {
		t.Fatal(err)
	}
	private.Channel = "ops" // Same label as a real public topic must still be hidden.
	if _, err := db.MessagingStore().Send(ctx, private); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, reader)
	if err != nil || len(list) != 1 || list[0].Name != "ops" || list[0].Address != env.To.URN() {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	page, err := svc.History(ctx, "ops", reader, 0, 1)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != first.ID {
		t.Fatalf("history=%+v err=%v", page, err)
	}
	seq := page.NextSince
	if page.Messages[0].DeliveredAt != nil || page.Messages[0].ConsumedAt != nil {
		t.Fatal("history changed delivery state")
	}
	_, tracked, err := db.DeliveryIDForMessage(ctx, first.ID)
	if err != nil || tracked {
		t.Fatalf("topic has delivery obligation: %v %v", tracked, err)
	}
	// Durable history and replay cursor survive closing the original process.
	path := filepath.Join(t.TempDir(), "reopened.db")
	// SQLite VACUUM INTO captures the whole primary DB, not a derived report.
	if _, err := db.DB().Exec("VACUUM INTO ?", path); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	other := channels.New(reopened, nil)
	second, err := other.Publish(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	page, err = other.History(ctx, "ops", reader, seq, 100)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != second.ID || page.NextSince <= seq {
		t.Fatalf("after reopen: %+v %v", page, err)
	}
	empty, err := other.History(ctx, "secret", reader, 0, 100)
	if err != nil || len(empty.Messages) != 0 {
		t.Fatalf("private label leaked: %+v %v", empty, err)
	}
}

func TestChannelReplayBackpressureAndLiveBoundary(t *testing.T) {
	_, svc := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := publication(t, "ops")
	var expected []string
	for range 105 {
		msg, err := svc.Publish(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, msg.ID)
	}
	zero := int64(0)
	replay, err := svc.Subscribe(ctx, "ops", reader, &zero)
	if err != nil {
		t.Fatal(err)
	}
	// Publish while the replay's unbuffered output is backpressured.
	final, err := svc.Publish(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	expected = append(expected, final.ID)
	var last int64
	for _, id := range expected {
		msg := receive(t, replay)
		if msg.ID != id || msg.Seq <= last {
			t.Fatalf("expected %s after %d, got %+v", id, last, msg)
		}
		last = msg.Seq
	}
	live, err := svc.Subscribe(ctx, "ops", reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	initial := <-live
	if initial.InitialCursor == nil || *initial.InitialCursor != last {
		t.Fatalf("initial cursor=%+v", initial)
	}
	next, err := svc.Publish(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if msg := receive(t, live); msg.ID != next.ID {
		t.Fatalf("live replayed old message: %s", msg.ID)
	}
	cancel()
	for range replay {
	} // Cancellation closes even a backpressured stream.
	for range live {
	}
}

func TestChannelCallerPolicyUsesVerifiedIdentity(t *testing.T) {
	db, _ := open(t)
	var operations []string
	p := identity.Principal{ID: "verified", Kind: "session", SessionID: "s", Scopes: []string{"read"}}
	svc := channels.New(db, func(ctx context.Context, operation, name string, caller identity.Principal, from gomsg.Address) error {
		if caller.ID != p.ID || caller.SessionID != p.SessionID {
			t.Errorf("unverified selector displaced caller: %+v", caller)
		}
		operations = append(operations, operation)
		if operation == "publish" {
			return channels.ErrForbidden
		}
		return nil
	})
	ctx := identity.WithPrincipal(context.Background(), p)
	if _, err := svc.List(ctx, "msg://user/local/spoof"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.History(ctx, "ops", "", 0, 1); err != nil {
		t.Fatal(err)
	}
	subctx, cancel := context.WithCancel(ctx)
	stream, err := svc.Subscribe(subctx, "ops", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range stream {
	}
	if _, err := svc.Publish(ctx, publication(t, "ops")); !errors.Is(err, channels.ErrForbidden) {
		t.Fatalf("policy not applied: %v", err)
	}
	if len(operations) != 4 {
		t.Fatalf("policy operations: %v", operations)
	}
	if _, err := channels.New(db, nil).List(context.Background(), ""); !errors.Is(err, channels.ErrInvalid) {
		t.Fatalf("anonymous claim: %v", err)
	}
}

func TestChannelRejectsConflictingPublicationAndClampsFutureCursor(t *testing.T) {
	db, svc := open(t)
	env := publication(t, "ops")
	env.Channel = "other"
	if _, err := db.MessagingStore().Send(context.Background(), env); !errors.Is(err, channels.ErrInvalid) {
		t.Fatalf("conflict=%v", err)
	}
	future := int64(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := svc.Subscribe(ctx, "ops", reader, &future); err != nil {
		t.Fatalf("future=%v", err)
	}
}

func TestChannelRetentionPurgesBodiesWithReceiptAndKeepsReplay(t *testing.T) {
	db, svc := open(t)
	ctx := context.Background()
	msg, err := svc.Publish(ctx, publication(t, "ops"))
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := db.ListRetentionCandidates(ctx, time.Now().Add(time.Hour))
	if err != nil || len(candidates) != 1 || !candidates[0].Eligible || candidates[0].HasDelivery {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	purged, err := db.PurgeMessageBody(ctx, msg.ID, reader)
	if err != nil || !purged {
		t.Fatalf("purge=%v %v", purged, err)
	}
	var id, author string
	if err := db.DB().QueryRow(`SELECT message_id,authorized_by FROM message_purge_audit`).Scan(&id, &author); err != nil || id != msg.ID || author != reader {
		t.Fatalf("receipt=%s %s %v", id, author, err)
	}
	page, err := svc.History(ctx, "ops", reader, 0, 100)
	if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Payload) != 0 || page.Messages[0].ID != msg.ID || page.NextSince == 0 || !page.Messages[0].Purged || page.Messages[0].PurgedAt == nil {
		t.Fatalf("purge destroyed history: %+v %v", page, err)
	}
	if again, err := db.PurgeMessageBody(ctx, msg.ID, reader); err != nil || again {
		t.Fatalf("purge not idempotent: %v %v", again, err)
	}
	// A private mailbox with this label keeps its real pending obligation.
	private := publication(t, "ops")
	private.To = gomsg.Address{Kind: gomsg.KindAgent, Authority: "local", ID: "private"}
	private.Channel = "ops"
	pending, err := db.MessagingStore().Send(ctx, private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PurgeMessageBody(ctx, pending.ID, reader); !errors.Is(err, store.ErrPendingObligation) {
		t.Fatalf("private pending body purged: %v", err)
	}
}

func TestChannelMailboxOperationsLeavePublicationUntouched(t *testing.T) {
	db, svc := open(t)
	ctx := context.Background()
	msg, err := svc.Publish(ctx, publication(t, "ops"))
	if err != nil {
		t.Fatal(err)
	}
	ms := db.MessagingStore()
	for name, call := range map[string]func() error{
		"inbox":     func() error { _, err := ms.Inbox(ctx, msg.To, gomsg.Filter{}); return err },
		"list":      func() error { _, err := ms.List(ctx, msg.To, store.ListFilter{}); return err },
		"subscribe": func() error { _, err := ms.Subscribe(ctx, msg.To, gomsg.Filter{}); return err },
		"consume":   func() error { return ms.Consume(ctx, msg.ID, msg.To) },
		"cancel":    func() error { return ms.Cancel(ctx, msg.ID) },
		"read":      func() error { return ms.MarkRead(ctx, msg.ID, msg.To) },
		"archive":   func() error { return ms.Archive(ctx, msg.ID, msg.To) },
		"unarchive": func() error { return ms.Unarchive(ctx, msg.ID, msg.To) },
	} {
		if err := call(); !errors.Is(err, channels.ErrMailboxOperation) {
			t.Errorf("%s = %v", name, err)
		}
	}
	var changed int
	if err := db.DB().QueryRow(`SELECT count(*) FROM messages WHERE id=? AND (delivered_at IS NOT NULL OR consumed_at IS NOT NULL OR canceled_at IS NOT NULL OR read_at IS NOT NULL OR archived_at IS NOT NULL)`, msg.ID).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("lifecycle changed=%d err=%v", changed, err)
	}
	page, err := svc.History(ctx, "ops", reader, 0, 100)
	if err != nil || len(page.Messages) != 1 || string(page.Messages[0].Payload) != string(msg.Payload) {
		t.Fatalf("history=%+v %v", page, err)
	}
	result, err := store.ImportLegacyMessagesIntoDelivery(ctx, db.DB(), db.DeliveryStore(), "")
	if err != nil {
		t.Fatal(err)
	}
	var deliveries int
	if err := db.DB().QueryRow(`SELECT count(*) FROM messaging_deliveries`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("import=%+v deliveries=%d err=%v", result, deliveries, err)
	}
}

func TestChannelHistoryUsesInsertionOrderAndReplaysPurgeReceipt(t *testing.T) {
	db, svc := open(t)
	ctx := context.Background()
	first, err := svc.Publish(ctx, publication(t, "ops"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Publish(ctx, publication(t, "ops"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE messages SET created_at=? WHERE id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), second.ID); err != nil {
		t.Fatal(err)
	}
	page, err := svc.History(ctx, "ops", reader, 0, 100)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].ID != first.ID || page.Messages[1].ID != second.ID {
		t.Fatalf("order=%+v %v", page, err)
	}
	if _, err := db.PurgeMessageBody(ctx, first.ID, reader); err != nil {
		t.Fatal(err)
	}
	replayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	zero := int64(0)
	stream, err := svc.Subscribe(replayCtx, "ops", reader, &zero)
	if err != nil {
		t.Fatal(err)
	}
	event := <-stream
	if event.Err != nil || !event.Message.Purged || event.Message.PurgedAt == nil || event.Message.ID != first.ID {
		t.Fatalf("purge replay=%+v", event)
	}
}

func TestChannelPublicationAndCursorCommitAtomically(t *testing.T) {
	db, svc := open(t)
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_publication BEFORE INSERT ON channel_publications BEGIN SELECT RAISE(ABORT,'cursor unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(context.Background(), publication(t, "ops")); err == nil {
		t.Fatal("publication succeeded without cursor")
	}
	var messageExists bool
	if err := db.DB().QueryRow(`SELECT EXISTS(SELECT 1 FROM messages WHERE to_urn='msg://service/local/channel/ops')`).Scan(&messageExists); err != nil {
		t.Fatal(err)
	}
	if messageExists {
		t.Fatal("failed publication left a durable unindexed envelope")
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_publication`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(context.Background(), publication(t, "ops")); err != nil {
		t.Fatal(err)
	}
	page, err := svc.History(context.Background(), "ops", reader, 0, 100)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("retry=%+v %v", page, err)
	}
}
