package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/spf13/cobra"
)

// Exercise the CLI command -> configured daemon client -> bearer middleware ->
// recipient handoff chain. These callers do not pass as_session; the daemon's
// verified session identity and stored actor association supply ownership.
func TestCLIRecipientDeliveryTransportWithoutAsSession(t *testing.T) {
	for _, path := range []string{"watch_stream", "list", "get", "inbox"} {
		for _, recipient := range []bool{true, false} {
			t.Run(path+map[bool]string{true: "/recipient", false: "/operator"}[recipient], func(t *testing.T) {
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
				h := identity.MiddlewareWithScopes(identity.Enforce, ids, nil, identity.ScopePolicy{}, api.NewHandler(api.Deps{MessageStore: db.MessagingStore(), Service: &serviceAdapter{svc: &app.Service{Store: db}}}))
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("as_session") != "" {
						t.Error("CLI unexpectedly relies on as_session")
					}
					h.ServeHTTP(w, r)
				}))
				defer srv.Close()
				tokenPath := filepath.Join(t.TempDir(), "session.token")
				if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
					t.Fatal(err)
				}
				oldCatalog, oldToken, oldAs := catalogPath, tokenFilePath, messageAs
				oldJSON, oldUnread, oldMailboxJSON := messageJSONFlag, messageUnreadOnly, mailboxJSONFlag
				oldAfter := mailboxAfterSeq
				afterFlag := mailboxWatchCmd.Flags().Lookup("after-seq")
				oldAfterChanged := afterFlag.Changed
				catalogPath = writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
				tokenFilePath = tokenPath
				messageAs = to.URN()
				messageJSONFlag = true
				messageUnreadOnly = true
				mailboxJSONFlag = true
				mailboxAfterSeq = 0
				afterFlag.Changed = true
				defer func() {
					catalogPath, tokenFilePath, messageAs = oldCatalog, oldToken, oldAs
					messageJSONFlag, messageUnreadOnly, mailboxJSONFlag = oldJSON, oldUnread, oldMailboxJSON
					mailboxAfterSeq = oldAfter
					afterFlag.Changed = oldAfterChanged
				}()
				cmd, args := map[string]*cobra.Command{"watch_stream": mailboxWatchCmd, "list": messageListCmd, "get": messageGetCmd, "inbox": messageInboxCmd}[path], []string{to.URN()}
				if path == "get" {
					args = []string{env.ID}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				oldCtx := cmd.Context()
				cmd.SetContext(ctx)
				defer cmd.SetContext(oldCtx)
				output := captureStdout(t, func() {
					if err := cmd.RunE(cmd, args); err != nil {
						t.Fatal(err)
					}
				})
				if !strings.Contains(output, env.ID) {
					t.Fatal("CLI did not receive the message")
				}
				got, err := db.MessagingStore().Get(context.Background(), env.ID)
				if err != nil {
					t.Fatal(err)
				}
				if (got.DeliveredAt != nil) != recipient {
					t.Fatalf("delivery=%v recipient=%v", got.DeliveredAt, recipient)
				}
				var count int
				if err := db.DB().QueryRow(`SELECT count(*) FROM message_event_journal WHERE kind='message.delivered' AND json_extract(payload_json,'$.path')=?`, path).Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 0
				if recipient {
					want = 1
				}
				if count != want {
					t.Fatalf("events=%d want=%d", count, want)
				}
			})
		}
	}
}
