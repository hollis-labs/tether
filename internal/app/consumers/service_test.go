package consumers_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/app/consumers"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

const consumer = "msg://agent/local/consumer"

func TestConsumerRoutedOutputReplaySurvivesReopenWithoutConsumption(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "consumer.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	channelService := channels.New(st, nil)
	reader := consumers.ConsumerService{ChannelReader: channelService}
	to, err := channels.ChannelAddress("agent-turns")
	if err != nil {
		t.Fatal(err)
	}
	env := messaging.Envelope{
		Kind:     messaging.MsgKindNotice,
		From:     messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "hosted-session"},
		To:       to,
		Payload:  json.RawMessage(`{"text":"full routed output","kind":"final","turn_id":"turn-1"}`),
		Metadata: map[string]string{"session_id": "hosted-session", "turn_id": "turn-1"},
	}
	first, err := channelService.Publish(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.Read(ctx, "agent-turns", consumer, 0, 1, 0)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != first.ID {
		t.Fatalf("initial output = %+v, err = %v", page, err)
	}
	if string(page.Messages[0].Payload) != string(env.Payload) || page.Messages[0].Metadata["turn_id"] != "turn-1" {
		t.Fatalf("consumer changed output body or attribution: %+v", page.Messages[0])
	}
	cursor := page.NextSince
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	channelService = channels.New(st, nil)
	reader = consumers.ConsumerService{ChannelReader: channelService}
	env.Payload = json.RawMessage(`{"text":"next routed output","kind":"final","turn_id":"turn-2"}`)
	env.Metadata["turn_id"] = "turn-2"
	second, err := channelService.Publish(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	page, err = reader.Read(ctx, "agent-turns", consumer, cursor, 100, 0)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != second.ID || page.NextSince <= cursor {
		t.Fatalf("resumed output = %+v, err = %v; want only the new publication", page, err)
	}
	latest, err := reader.Read(ctx, "agent-turns", consumer, 0, 0, 1)
	if err != nil || len(latest.Messages) != 1 || latest.Messages[0].ID != second.ID || latest.NextSince != page.NextSince {
		t.Fatalf("latest output = %+v, err = %v", latest, err)
	}
	for _, id := range []string{first.ID, second.ID} {
		stored, err := st.MessagingStore().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ConsumedAt != nil || stored.DeliveredAt != nil {
			t.Fatalf("observing output changed receipt state: %+v", stored)
		}
	}
}

func TestConsumerReadPreservesDaemonAuthorization(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "authorization.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	denied := errors.New("daemon denied channel read")
	channelService := channels.New(st, func(_ context.Context, operation, name string, caller identity.Principal, _ messaging.Address) error {
		if caller.ID != consumer || (operation == "history" && name != "agent-turns") {
			t.Fatalf("authorization received wrong actor/channel: %s %s %+v", operation, name, caller)
		}
		return denied
	})
	reader := consumers.ConsumerService{ChannelReader: channelService}
	ctx := context.Background()
	if _, err := reader.ListPage(ctx, consumer, 0, 100); !errors.Is(err, denied) {
		t.Fatalf("list error = %v; want daemon denial", err)
	}
	if _, err := reader.Read(ctx, "agent-turns", consumer, 0, 100, 0); !errors.Is(err, denied) {
		t.Fatalf("history error = %v; want daemon denial", err)
	}
	if _, err := reader.Read(ctx, "agent-turns", consumer, 0, 0, 1); !errors.Is(err, denied) {
		t.Fatalf("latest error = %v; want daemon denial", err)
	}
	if _, err := reader.Read(ctx, "agent-turns", "", 0, 100, 0); !errors.Is(err, channels.ErrInvalid) {
		t.Fatalf("missing actor error = %v; want invalid identity", err)
	}
}
