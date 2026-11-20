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

func TestMCPRecipientListAndGetRecordDelivery(t *testing.T) {
	for _, tool := range []string{"tether_message_list", "tether_message_get"} {
		for _, recipient := range []bool{true, false} {
			t.Run(tool+map[bool]string{true: "/recipient", false: "/operator"}[recipient], func(t *testing.T) {
				db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
				env, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}, To: to})
				if err != nil {
					t.Fatal(err)
				}
				h := api.NewHandler(api.Deps{MessageStore: db.MessagingStore()})
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p := identity.Principal{Verified: true, Kind: "session", ID: to.URN(), SessionID: "s-worker"}
					if !recipient {
						p = identity.Principal{Verified: true, Kind: "operator", ID: identity.OperatorID, Scopes: []string{"*"}}
					}
					h.ServeHTTP(w, r.WithContext(identity.WithPrincipal(r.Context(), p)))
				}))
				defer srv.Close()
				a := NewWithDaemon(&app.Service{}, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://")), "test", []string{"read"})
				res := callAnyTool(t, a, tool, map[string]any{"message_id": env.ID, "as": to.URN(), "to": to.URN(), "unread_only": true})
				if res.IsError {
					t.Fatal(textOf(res))
				}
				got, err := db.MessagingStore().Get(context.Background(), env.ID)
				if err != nil {
					t.Fatal(err)
				}
				if (got.DeliveredAt != nil) != recipient {
					t.Fatalf("delivery=%v recipient=%v", got.DeliveredAt, recipient)
				}
			})
		}
	}
}
