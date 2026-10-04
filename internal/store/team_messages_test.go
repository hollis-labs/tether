package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

func TestKeyedTeamMailboxRepairsContentAfterEnqueueAndDeduplicates(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	from, err := messaging.ParseURN("msg://agent/local/worker")
	if err != nil {
		t.Fatal(err)
	}
	to, err := messaging.ParseURN("msg://user/local/operator")
	if err != nil {
		t.Fatal(err)
	}
	envelope := messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: []byte(`{"body":"result"}`)}
	if _, err = s.DB().Exec(`CREATE TRIGGER fail_team_content BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'lost content ack'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendKeyedMessage(context.Background(), "delivery", envelope); err == nil {
		t.Fatal("content write failure ignored")
	}
	if _, err = s.DB().Exec(`DROP TRIGGER fail_team_content`); err != nil {
		t.Fatal(err)
	}
	first, err := s.SendKeyedMessage(context.Background(), "delivery", envelope)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SendKeyedMessage(context.Background(), "delivery", envelope)
	if err != nil || again.ID != first.ID {
		t.Fatal("keyed replay changed message", again, err)
	}
	var count int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	changed := envelope
	changed.Payload = []byte(`{"body":"changed"}`)
	if _, err = s.SendKeyedMessage(context.Background(), "delivery", changed); err == nil {
		t.Fatal("changed content accepted")
	}
	if _, err = s.MessagingStore().Get(context.Background(), first.ID); errors.Is(err, messaging.ErrNotFound) {
		t.Fatal("repair did not materialize content")
	}
}

func TestKeyedTeamMailboxRefusesPublicationWithPermanentArgumentClass(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	from, err := messaging.ParseURN("msg://agent/local/worker")
	if err != nil {
		t.Fatal(err)
	}
	to, err := channels.ChannelAddress("topic")
	if err != nil {
		t.Fatal(err)
	}
	env := messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: []byte(`{"body":"publication"}`)}
	if _, err = s.SendKeyedMessage(context.Background(), "publication", env); !errors.Is(err, delivery.ErrInvalidArgument) {
		t.Fatal("publication entered keyed mailbox", err)
	}
	env.Channel = "different"
	if _, err = s.SendKeyedMessage(context.Background(), "invalid-publication", env); !errors.Is(err, delivery.ErrInvalidArgument) {
		t.Fatal("invalid publication stayed retryable", err)
	}
	if _, err = s.SendKeyedMessage(context.Background(), "", env); !errors.Is(err, delivery.ErrInvalidArgument) {
		t.Fatal("empty key stayed retryable", err)
	}
}
