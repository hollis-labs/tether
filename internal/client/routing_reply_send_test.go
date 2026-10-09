package client

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

// repliesToStore is the smallest real reply service: it stores the reply like
// the daemon does and reports it queued.
type repliesToStore struct{ db *store.Store }

func (r repliesToStore) SubmitRoutingReply(ctx context.Context, req api.RoutingReplyRequest) (api.RoutingReplyReceipt, error) {
	caller, err := messaging.ParseURN(req.Caller.ID)
	if err != nil {
		return api.RoutingReplyReceipt{}, err
	}
	reply, _, err := r.db.CreateRoutingReply(ctx, store.NewRoutingReply{From: caller, ParentID: req.ParentID, Body: req.Body,
		TargetSessionID: "s1", Actor: caller.URN()})
	if err != nil {
		return api.RoutingReplyReceipt{}, err
	}
	return api.RoutingReplyReceipt{ReplyID: reply.ReplyID, ParentID: req.ParentID, State: "queued", TargetSessionID: "s1"}, nil
}

func (r repliesToStore) RoutingReplyDelivery(context.Context, string) (api.RoutingReplyDelivery, error) {
	return api.RoutingReplyDelivery{}, api.ErrReplyNotFound
}

// The shared client (so the MCP tether_message_send tool and `tether message
// send`) accepts only 201 from POST /messages. A send whose in_reply_to names a
// routed message is queued for its sender session; it must still read as a sent
// message, or an agent would see an error for a reply that was accepted and retry.
func TestMessageSendWithInReplyToARoutedMessageReadsAsAnOrdinarySend(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "reply.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	channel, _ := channels.ChannelAddress("ops")
	parent, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, To: channel, Payload: []byte(`{"text":"which?"}`)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(api.Deps{MessageStore: db.MessagingStore(), Channels: channels.New(db, nil), RoutingReplies: repliesToStore{db}}))
	t.Cleanup(srv.Close)

	c := New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	sent, err := c.MessageSend(context.Background(), MessageSendRequest{
		From: "msg://user/local/chris", Kind: "response", InReplyTo: parent.ID, Payload: []byte(`"the second option"`)})
	if err != nil {
		t.Fatalf("the client rejected an accepted reply: %v", err)
	}
	if sent.ID == "" || sent.InReplyTo != parent.ID {
		t.Fatalf("sent = %+v", sent)
	}
	stored, err := db.RoutingReply(context.Background(), sent.ID)
	if err != nil || stored.State != store.RoutingReplyQueued || stored.TargetSessionID != "s1" {
		t.Fatalf("the returned id must be the queued reply: %+v %v", stored, err)
	}
}
