package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestRecipientDeliveryHTTPPaths(t *testing.T) {
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "recipient"}
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "sender"}
	for _, path := range []string{"list", "get", "inbox", "watch_stream", "operator-declared-watch"} {
		for _, actor := range []string{"recipient", "bound_jwt", "unrelated_jwt", "sender", "admin", "scoped_admin", "anonymous"} {
			t.Run(path+"/"+actor, func(t *testing.T) {
				db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				ms := db.MessagingStore()
				env, err := ms.Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
				if err != nil {
					t.Fatal(err)
				}
				svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{
					"s-own":   {ID: "s-own", LogicalAgentID: to.ID},
					"s-other": {ID: "s-other", LogicalAgentID: from.ID},
				}}
				h := NewHandler(Deps{MessageStore: ms, Service: svc})
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p := identity.Principal{Verified: true, Kind: "session", ID: from.URN(), SessionID: "s1"}
					if actor == "recipient" {
						p.ID = to.URN()
					}
					if actor == "bound_jwt" || actor == "unrelated_jwt" {
						sid := "s-own"
						if actor == "unrelated_jwt" {
							sid = "s-other"
						}
						p = identity.Principal{Verified: true, Kind: "agent", ID: identity.SessionPrincipalIDPrefix + sid, SessionID: sid, TokenID: "issued", Scopes: []string{"message.send_as"}}
					}
					if actor == "admin" || actor == "scoped_admin" {
						p.Kind = "operator"
						p.ID = identity.OperatorID
						p.Scopes = []string{"*"}
						if actor == "scoped_admin" {
							p.TokenID = "narrow"
						}
					}
					if actor != "anonymous" {
						r = r.WithContext(identity.WithPrincipal(r.Context(), p))
					}
					h.ServeHTTP(w, r)
				}))
				defer srv.Close()
				q := url.Values{"as": {to.URN()}, "to": {to.URN()}, "unread_only": {"true"}}
				endpoint := "/messages/" + path
				if path == "get" {
					endpoint = "/messages/" + env.ID
					if actor == "sender" {
						q.Set("as", from.URN())
					}
				}
				if path == "watch_stream" || path == "operator-declared-watch" {
					endpoint = "/messages/watch"
					q.Set("after_seq", "0")
					if path == "operator-declared-watch" {
						q.Set("as_recipient", "true")
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+endpoint+"?"+q.Encode(), nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if path == "operator-declared-watch" && actor != "admin" && actor != "recipient" && actor != "bound_jwt" {
					if resp.StatusCode != http.StatusForbidden {
						t.Fatalf("status=%d want 403", resp.StatusCode)
					}
					resp.Body.Close()
					got, _ := ms.Get(context.Background(), env.ID)
					if got.DeliveredAt != nil {
						t.Fatal("forbidden watch stamped delivery")
					}
					return
				}
				if resp.StatusCode != 200 {
					t.Fatalf("status=%d", resp.StatusCode)
				}
				if path != "watch_stream" && path != "operator-declared-watch" {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				// The stream stamps only after its flush succeeds; allow that server-side step.
				deadline := time.Now().Add(time.Second)
				for {
					got, err := ms.Get(context.Background(), env.ID)
					if err != nil {
						t.Fatal(err)
					}
					if (got.DeliveredAt != nil) == (actor == "recipient" || actor == "bound_jwt" || (actor == "admin" && path == "operator-declared-watch")) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("delivered=%v actor=%s", got.DeliveredAt, actor)
					}
					time.Sleep(time.Millisecond)
				}
				cancel()
				_ = resp.Body.Close()
				var count int
				eventPath := path
				if path == "operator-declared-watch" && actor != "admin" {
					eventPath = "watch_stream"
				}
				if err := db.DB().QueryRow(`SELECT count(*) FROM message_event_journal WHERE kind='message.delivered' AND json_extract(payload_json,'$.path')=?`, eventPath).Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 0
				if actor == "recipient" || actor == "bound_jwt" || (actor == "admin" && path == "operator-declared-watch") {
					want = 1
				}
				if count != want {
					t.Fatalf("events=%d want=%d", count, want)
				}
			})
		}
	}
}

type failedDeliveryWriter struct {
	header    http.Header
	failFlush bool
}

func (w *failedDeliveryWriter) Header() http.Header { return w.header }
func (w *failedDeliveryWriter) WriteHeader(int)     {}
func (w *failedDeliveryWriter) Write(p []byte) (int, error) {
	if w.failFlush {
		return len(p), nil
	}
	return 0, io.ErrClosedPipe
}
func (w *failedDeliveryWriter) Flush()            {}
func (w *failedDeliveryWriter) FlushError() error { return io.ErrClosedPipe }

func TestWatchFailedWriteOrFlushDoesNotDeliver(t *testing.T) {
	for _, operator := range []bool{false, true} {
		for _, failFlush := range []bool{false, true} {
			t.Run(fmt.Sprint(operator)+"/"+map[bool]string{false: "write", true: "flush"}[failFlush], func(t *testing.T) {
				db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				to := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "worker"}
				env, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: to, To: to})
				if err != nil {
					t.Fatal(err)
				}
				q := url.Values{"to": {to.URN()}, "as": {to.URN()}, "after_seq": {"0"}}
				if operator {
					q.Set("as_recipient", "true")
				}
				req := httptest.NewRequest(http.MethodGet, "/messages/watch?"+q.Encode(), nil)
				p := identity.Principal{Verified: true, Kind: "session", ID: to.URN()}
				if operator {
					p = identity.Principal{Verified: true, Kind: "operator", ID: identity.OperatorID, Scopes: []string{"*"}}
				}
				req = req.WithContext(identity.WithPrincipal(req.Context(), p))
				NewHandler(Deps{MessageStore: db.MessagingStore()}).ServeHTTP(&failedDeliveryWriter{header: make(http.Header), failFlush: failFlush}, req)
				got, err := db.MessagingStore().Get(context.Background(), env.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.DeliveredAt != nil {
					t.Fatalf("failed %v stamped delivery", failFlush)
				}
			})
		}
	}
}

func TestOperatorDeclaredWatchReplayAndAudit(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ms := db.MessagingStore()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "worker"}
	env, err := ms.Send(t.Context(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: to, To: to})
	if err != nil {
		t.Fatal(err)
	}
	audits := make(chan identity.Observation, 4)
	h := NewHandler(Deps{MessageStore: ms, IdentityAudit: func(_ context.Context, o identity.Observation) error { audits <- o; return nil }})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(identity.WithPrincipal(r.Context(), identity.Principal{Verified: true, Kind: "operator", ID: identity.OperatorID, Scopes: []string{"*"}})))
	}))
	defer srv.Close()
	var first time.Time
	for i := 0; i < 2; i++ {
		q := watchQuery(to, "0")
		q.Set("as_recipient", "true")
		frames, stop := watchOpen(t, srv.URL, q)
		if f := nextFrame(t, frames); f.event != "message" {
			t.Fatalf("frame=%+v", f)
		}
		if f := nextFrame(t, frames); f.event != "synchronized" {
			t.Fatalf("frame=%+v", f)
		}
		stop()
		got, err := ms.Get(t.Context(), env.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.DeliveredAt == nil || got.ConsumedAt != nil {
			t.Fatalf("state=%+v", got)
		}
		page, err := ms.List(t.Context(), to, store.ListFilter{})
		if err != nil || len(page.Messages) != 1 || page.Messages[0].ReadAt != nil {
			t.Fatalf("read state changed: %+v, %v", page, err)
		}
		if i == 0 {
			first = *got.DeliveredAt
		} else if !first.Equal(*got.DeliveredAt) {
			t.Fatal("replay restamped delivery")
		}
		select {
		case o := <-audits:
			if o.Event != "operator_declared_watch" || o.PrincipalID != identity.OperatorID || !strings.Contains(o.Detail, to.URN()) {
				t.Fatalf("audit=%+v", o)
			}
		case <-time.After(time.Second):
			t.Fatal("missing declaration audit")
		}
	}
	var count int
	if err := db.DB().QueryRow(`SELECT count(*) FROM message_event_journal WHERE kind='message.delivered' AND json_extract(payload_json,'$.path')='operator-declared-watch'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("delivery events=%d want 1", count)
	}
}
