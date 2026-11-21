package mcpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestMCPRecipientPayloadReadsRecordDeliveryAndRead(t *testing.T) {
	for _, tool := range []string{"tether_message_list", "tether_message_get", "tether_message_inbox", "tether_message_watch"} {
		for _, recipient := range []bool{true, false} {
			t.Run(tool+map[bool]string{true: "/recipient", false: "/operator"}[recipient], func(t *testing.T) {
				db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				to := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "worker"}
				env, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "sender"}, To: to})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.CreateSession(store.SessionRow{ID: "s-worker", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
					t.Fatal(err)
				}
				ids := identity.NewStore(db.DB())
				p := identity.Principal{Kind: "session", ID: identity.SessionPrincipalIDPrefix + "s-worker", SessionID: "s-worker", Scopes: []string{"read", "message.send_as"}, Addresses: []string{identity.SessionPrincipalIDPrefix + "s-worker"}}
				if !recipient {
					p = identity.Principal{Kind: "operator", ID: identity.OperatorID, Scopes: []string{"*"}}
				}
				token, err := ids.Mint(context.Background(), p)
				if err != nil {
					t.Fatal(err)
				}
				h := identity.MiddlewareWithScopes(identity.Enforce, ids, nil, identity.ScopePolicy{}, api.NewHandler(api.Deps{MessageStore: db.MessagingStore(), Service: &daemonOnlyService{db: db}}))
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tool != "tether_message_inbox" && r.URL.Query().Get("as_session") != "" {
						t.Error("list/get unexpectedly relies on as_session")
					}
					h.ServeHTTP(w, r)
				}))
				defer srv.Close()
				a := NewWithDaemon(&app.Service{}, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(token)), "test", []string{"read"})
				a.SessionID = "s-worker"

				res := callAnyTool(t, a, tool, map[string]any{"message_id": env.ID, "as": to.URN(), "to": to.URN(), "unread_only": true, "after_seq": float64(0), "as_recipient": tool == "tether_message_watch" && !recipient})
				if res.IsError {
					t.Fatal(textOf(res))
				}
				got, err := db.MessagingStore().Get(context.Background(), env.ID)
				if err != nil {
					t.Fatal(err)
				}
				if (got.DeliveredAt != nil) != (recipient || tool == "tether_message_watch") {
					t.Fatalf("delivery=%v recipient=%v", got.DeliveredAt, recipient)
				}
				page, err := db.MessagingStore().List(t.Context(), to, store.ListFilter{})
				if err != nil || len(page.Messages) != 1 {
					t.Fatalf("page=%+v err=%v", page, err)
				}
				if (page.Messages[0].ReadAt != nil) != (recipient && tool != "tether_message_watch") {
					t.Fatalf("read=%v recipient=%v tool=%s", page.Messages[0].ReadAt, recipient, tool)
				}
			})
		}
	}
}
