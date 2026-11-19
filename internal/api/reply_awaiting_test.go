package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
)

// CW-20261117-0003: an untyped reply names the asker in its awaiting metadata.
func TestUntypedReplyAwaitsTheAsker(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	asker, answerer := claimAddr("asker"), claimAddr("answerer")
	ask, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindRequest, From: asker, To: answerer, Payload: json.RawMessage(`{"body":"please"}`)})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := postJSON(t, srv.URL+"/messages", map[string]any{"kind": "response", "from": answerer, "to": asker, "in_reply_to": ask.ID, "payload": map[string]string{"body": "done"}})
	if resp.StatusCode/100 != 2 {
		t.Fatalf("send: %d %s", resp.StatusCode, body)
	}
	var sent messaging.Envelope
	if err := json.Unmarshal(body, &sent); err != nil || sent.ID == "" {
		var wrap struct {
			Message messaging.Envelope `json:"message"`
		}
		if json.Unmarshal(body, &wrap) != nil || wrap.Message.ID == "" {
			t.Fatalf("decode %s", body)
		}
		sent = wrap.Message
	}
	stored, err := db.MessagingStore().Get(ctx, sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Metadata[MetadataAwaiting]; got != asker.URN() {
		t.Fatalf("awaiting = %q, want %q", got, asker.URN())
	}
	// A follow-up to one's own message awaits nobody.
	resp, body = postJSON(t, srv.URL+"/messages", map[string]any{"kind": "notice", "from": asker, "to": answerer, "in_reply_to": ask.ID, "payload": map[string]string{"body": "also"}})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("follow-up: %d %s", resp.StatusCode, body)
	}
}
