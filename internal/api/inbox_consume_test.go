package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/messaging/delivery"

	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261001-0016: GET /messages/inbox consumes what it returns only when
// as_session is a live session that is the recipient itself. Every other
// listing stays a plain pull and settles nothing.
func TestInbox_ConsumesOnlyForTheRecipientsOwnSession(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := &fakeLaunchService{
		getRes: map[string]*store.SessionRow{
			"s-worker": {ID: "s-worker", LogicalAgentID: "worker"},
			"s-other":  {ID: "s-other", LogicalAgentID: "other"},
			"s-gone":   {ID: "s-gone", LogicalAgentID: "worker"},
		},
		runtimeHealthOK: map[string]bool{"s-worker": true, "s-other": true},
	}
	srv := httptest.NewServer(NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore()}))
	t.Cleanup(srv.Close)

	actor := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sessionAddr := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s-worker"}
	for _, tc := range []struct {
		name      string
		to        messaging.Address
		asSession string
		consumed  bool
	}{
		{"actor's own session", actor, "s-worker", true},
		{"session address, same session", sessionAddr, "s-worker", true},
		{"operator listing, no session", actor, "", false},
		{"another live session", actor, "s-other", false},
		{"the actor's session, not running", actor, "s-gone", false},
		{"unknown session", sessionAddr, "s-nobody", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ms := db.MessagingStore()
			sent, err := ms.Send(ctx, messaging.Envelope{
				Kind: messaging.MsgKindNotice,
				From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"},
				To:   tc.to,
			})
			if err != nil {
				t.Fatal(err)
			}

			q := url.Values{"to": {tc.to.URN()}, "as": {tc.to.URN()}}
			if tc.asSession != "" {
				q.Set("as_session", tc.asSession)
			}
			resp, err := http.Get(srv.URL + "/messages/inbox?" + q.Encode())
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body struct {
				Messages []messaging.Envelope `json:"messages"`
			}
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil || len(body.Messages) != 1 {
				t.Fatalf("inbox status=%d messages=%d; want 200 with the one message", resp.StatusCode, len(body.Messages))
			}

			got, err := ms.Get(ctx, sent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (got.ConsumedAt != nil) != tc.consumed {
				t.Fatalf("consumed_at = %v; want consumed=%v", got.ConsumedAt, tc.consumed)
			}
			deliveryID, ok, err := db.DeliveryIDForMessage(ctx, sent.ID)
			if err != nil || !ok {
				t.Fatalf("delivery id: ok=%v err=%v", ok, err)
			}
			rd, err := db.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
			if err != nil {
				t.Fatal(err)
			}
			if settled := rd.Status == delivery.DeliveryDelivered; settled != tc.consumed {
				t.Fatalf("delivery status = %q; want settled=%v", rd.Status, tc.consumed)
			}
		})
	}
}
