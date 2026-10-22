package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/store"
)

// boundLaunchService answers the optional SessionBoundToActor question from a map.
type boundLaunchService struct {
	*fakeLaunchService
	bound map[string]string // actor id -> the session its current binding resolves to
}

func (b boundLaunchService) SessionBoundToActor(sessionID, actorID string) bool {
	return b.bound[actorID] == sessionID
}

// CW-20261021-0029: an actor whose mailbox URN is an agt_ id (its session's logical agent id
// differs) is pulled as the recipient session only when the actor's current binding is that
// session: unread only, read mail settled, unread mail consumed. A session bound to a different
// actor, or an unbound one, gets the plain pull.
func TestInbox_ActorMailboxPullByTheBoundSession(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "inbox-bound.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := &fakeLaunchService{
		getRes: map[string]*store.SessionRow{
			"s-bound":   {ID: "s-bound", LogicalAgentID: "cw-verify"},
			"s-other":   {ID: "s-other", LogicalAgentID: "cw-verify"},
			"s-unbound": {ID: "s-unbound", LogicalAgentID: "cw-verify"},
		},
		runtimeHealthOK: map[string]bool{"s-bound": true, "s-other": true, "s-unbound": true},
	}
	svc := boundLaunchService{fakeLaunchService: base, bound: map[string]string{"agt_x": "s-bound"}}
	srv := httptest.NewServer(NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore()}))
	t.Cleanup(srv.Close)
	actor := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "agt_x"}

	for _, tc := range []struct {
		name      string
		asSession string
		recipient bool
	}{
		{"the session the actor is bound to", "s-bound", true},
		{"a live session bound to something else", "s-other", false},
		{"a live session with no binding", "s-unbound", false},
		{"operator pull, no session", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			ms := db.MessagingStore()
			send := func() messaging.Envelope {
				e, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}, To: actor})
				if err != nil {
					t.Fatal(err)
				}
				return e
			}
			read, unread := send(), send()
			if _, err := db.DB().Exec(`UPDATE messages SET read_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), read.ID); err != nil {
				t.Fatal(err)
			}
			q := url.Values{"to": {actor.URN()}, "as": {actor.URN()}}
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
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
				t.Fatalf("inbox status %d", resp.StatusCode)
			}
			got := map[string]bool{}
			for _, m := range body.Messages {
				got[m.ID] = true
			}
			if !got[unread.ID] {
				t.Fatalf("the unread message must always be returned: %v", got)
			}
			if got[read.ID] == tc.recipient {
				t.Fatalf("read message returned=%v, want %v (unread-only is for the recipient session only)", got[read.ID], !tc.recipient)
			}
			u, err := ms.Get(ctx, unread.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (u.ConsumedAt != nil) != tc.recipient {
				t.Fatalf("unread message consumed_at = %v, want consumed=%v", u.ConsumedAt, tc.recipient)
			}
		})
	}
}
