package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

type fakeReplies struct {
	mu       sync.Mutex
	requests []RoutingReplyRequest
	err      error
	delivery RoutingReplyDelivery
	// db, when set, stores the reply like the real service does and returns its id.
	db *store.Store
}

func (f *fakeReplies) SubmitRoutingReply(ctx context.Context, req RoutingReplyRequest) (RoutingReplyReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return RoutingReplyReceipt{}, f.err
	}
	id := "reply-1"
	if f.db != nil {
		caller, err := messaging.ParseURN(req.Caller.ID)
		if err != nil {
			return RoutingReplyReceipt{}, err
		}
		reply, _, err := f.db.CreateRoutingReply(ctx, store.NewRoutingReply{From: caller, ParentID: req.ParentID, Body: req.Body,
			TargetSessionID: "s1", Actor: caller.URN()})
		if err != nil {
			return RoutingReplyReceipt{}, err
		}
		id = reply.ReplyID
	}
	return RoutingReplyReceipt{ReplyID: id, ParentID: req.ParentID, State: "queued", TargetSessionID: "s1"}, nil
}

func (f *fakeReplies) RoutingReplyDelivery(_ context.Context, id string) (RoutingReplyDelivery, error) {
	if id != f.delivery.ReplyID {
		return RoutingReplyDelivery{}, ErrReplyNotFound
	}
	return f.delivery, nil
}

func replyServer(t *testing.T, svc RoutingReplyService) (http.Handler, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "reply.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewHandler(Deps{MessageStore: db.MessagingStore(), Channels: channels.New(db, nil), RoutingReplies: svc}), db
}

// routedMessage publishes a message a session sent to a channel.
func routedMessage(t *testing.T, db *store.Store) messaging.Envelope {
	t.Helper()
	to, _ := channels.ChannelAddress("ops")
	env, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, To: to,
		Payload: []byte(`{"text":"which option?"}`), ContentType: "application/json"})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func post(h http.Handler, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	return postAs(context.Background(), h, path, body, headers)
}

func postAs(ctx context.Context, h http.Handler, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)).WithContext(ctx)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestReplyEndpointAcceptsAReplyToAChannelMessage(t *testing.T) {
	svc := &fakeReplies{}
	h, db := replyServer(t, svc)
	parent := routedMessage(t, db)

	w := post(h, "/messages/"+parent.ID+"/reply?as=msg://user/local/chris", map[string]any{"body": "the second one", "interrupt": true},
		map[string]string{"Idempotency-Key": "k-1"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d body %s (the channel mailbox guard must not apply to reply)", w.Code, w.Body)
	}
	var receipt RoutingReplyReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.ReplyID != "reply-1" || receipt.State != "queued" || receipt.TargetSessionID != "s1" {
		t.Fatalf("receipt %+v %v", receipt, err)
	}
	got := svc.requests[0]
	if got.ParentID != parent.ID || got.Body != "the second one" || !got.Interrupt || got.IdempotencyKey != "k-1" ||
		got.Caller.ID != "msg://user/local/chris" || got.Verified {
		t.Fatalf("request %+v", got)
	}
}

func TestReplyEndpointPrefersTheVerifiedPrincipalOverAs(t *testing.T) {
	svc := &fakeReplies{}
	h, db := replyServer(t, svc)
	parent := routedMessage(t, db)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "msg://user/local/verified", Kind: "user"})
	w := postAs(ctx, h, "/messages/"+parent.ID+"/reply?as=msg://user/local/spoofed", map[string]any{"body": "x"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if got := svc.requests[0]; got.Caller.ID != "msg://user/local/verified" || !got.Verified {
		t.Fatalf("request %+v", got)
	}
}

func TestReplyEndpointRequiresAnIdentityAndABody(t *testing.T) {
	svc := &fakeReplies{}
	h, db := replyServer(t, svc)
	parent := routedMessage(t, db)
	for name, tc := range map[string]struct {
		path string
		body any
	}{
		"no as":        {"/messages/" + parent.ID + "/reply", map[string]any{"body": "x"}},
		"as not a urn": {"/messages/" + parent.ID + "/reply?as=chris", map[string]any{"body": "x"}},
	} {
		if w := post(h, tc.path, tc.body, nil); w.Code != http.StatusBadRequest || errorCode(t, w) != CodeInvalidRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/messages/"+parent.ID+"/reply?as=msg://user/local/chris", bytes.NewReader([]byte("{not json")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad json: %d", w.Code)
	}
	if len(svc.requests) != 0 {
		t.Fatalf("a malformed request reached the service: %+v", svc.requests)
	}
	getReq := httptest.NewRequest(http.MethodGet, "/messages/"+parent.ID+"/reply", nil)
	gw := httptest.NewRecorder()
	h.ServeHTTP(gw, getReq)
	if gw.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reply: %d", gw.Code)
	}
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error envelope: %s", w.Body)
	}
	return e.Error.Code
}

func TestReplyErrorsAreTypedAndCarryTheirOwnStatus(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrReplyInvalid, 400, CodeInvalidRequest},
		{fmt.Errorf("%w: 200000 bytes", ErrReplyTooLarge), 413, CodePayloadTooLarge},
		{ErrReplyParentNotFound, 404, CodeNotFound},
		{ErrReplyTargetNotSession, 400, CodeReplyTargetNotSession},
		{ErrReplyForbidden, 403, CodeForbidden},
		{ErrReplyInterruptUnsupported, 409, CodeInterruptUnsupported},
		{ErrReplyTurnNotStarted, 409, CodeTurnNotYetStarted},
		{ErrReplyIdempotencyConflict, 409, CodeIdempotencyConflict},
		{ErrRoutingRepliesNotWired, 501, CodeNotImplemented},
		{errors.New("disk on fire"), 500, CodeInternalError},
	}
	for _, tc := range cases {
		svc := &fakeReplies{err: tc.err}
		h, db := replyServer(t, svc)
		parent := routedMessage(t, db)
		w := post(h, "/messages/"+parent.ID+"/reply?as=msg://user/local/chris", map[string]any{"body": "x"}, nil)
		if w.Code != tc.status || errorCode(t, w) != tc.code {
			t.Errorf("%v: got %d %s, want %d %s", tc.err, w.Code, w.Body, tc.status, tc.code)
		}
	}
}

func TestSendWithInReplyToARoutedMessageIsAReplyNotAMailboxDelivery(t *testing.T) {
	svc := &fakeReplies{}
	h, db := replyServer(t, svc)
	svc.db = db
	parent := routedMessage(t, db)

	w := post(h, "/messages", map[string]any{"kind": "response", "from": "msg://user/local/chris", "in_reply_to": parent.ID,
		"payload": map[string]string{"body": "use the second option"}}, nil)
	// 201 and an envelope, like any send: the MCP tool and the CLI read it unchanged.
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var sent struct {
		messaging.Envelope
		RoutingReply RoutingReplyReceipt `json:"routing_reply"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ID == "" || sent.InReplyTo != parent.ID || sent.RoutingReply.ReplyID != sent.ID || sent.RoutingReply.State != "queued" ||
		sent.RoutingReply.TargetSessionID != "s1" {
		t.Fatalf("response %+v", sent)
	}
	got := svc.requests[0]
	if got.ParentID != parent.ID || got.Body != "use the second option" || got.Caller.ID != "msg://user/local/chris" || got.Interrupt {
		t.Fatalf("request %+v", got)
	}
	// The only stored copy is the reply the service made; the HTTP layer added no mailbox row.
	var count int
	if err := db.DB().QueryRow(`SELECT count(*) FROM messages WHERE in_reply_to=?`, parent.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored replies = %d (%v), want exactly the service's one", count, err)
	}

	// to, when given, must be the sender session.
	other := post(h, "/messages", map[string]any{"kind": "response", "from": "msg://user/local/chris", "in_reply_to": parent.ID,
		"to": "msg://agent/local/someone-else", "payload": "x"}, nil)
	if other.Code != http.StatusBadRequest {
		t.Fatalf("to mismatch: %d %s", other.Code, other.Body)
	}
	same := post(h, "/messages", map[string]any{"kind": "response", "from": "msg://user/local/chris", "in_reply_to": parent.ID,
		"to": parent.From.URN(), "payload": "plain text"}, nil)
	if same.Code != http.StatusCreated || svc.requests[1].Body != "plain text" {
		t.Fatalf("to = sender session: %d %s %+v", same.Code, same.Body, svc.requests)
	}
}

func TestInReplyToAnOrdinaryMessageKeepsItsMailboxDelivery(t *testing.T) {
	svc := &fakeReplies{}
	h, db := replyServer(t, svc)
	// A message a session sent to an agent's mailbox, not to a channel.
	mailbox, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindRequest,
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "boss"}, Payload: []byte(`"ping"`)})
	if err != nil {
		t.Fatal(err)
	}
	w := post(h, "/messages", map[string]any{"kind": "response", "from": "msg://agent/local/boss", "to": "msg://session/local/s1",
		"in_reply_to": mailbox.ID, "payload": "pong"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if len(svc.requests) != 0 {
		t.Fatalf("an ordinary reply was rerouted: %+v", svc.requests)
	}
}

func TestSendWithInReplyToIsOrdinaryWhenReplyRoutingIsOff(t *testing.T) {
	h, db := replyServer(t, nil)
	parent := routedMessage(t, db)
	w := post(h, "/messages", map[string]any{"kind": "response", "from": "msg://user/local/chris", "to": "msg://agent/local/boss",
		"in_reply_to": parent.ID, "payload": "x"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
}

func TestDeliveryEndpoint(t *testing.T) {
	svc := &fakeReplies{delivery: RoutingReplyDelivery{ReplyID: "r1", ParentID: "p", State: "undeliverable", Reason: "session_ended_no_binding",
		OriginalSessionID: "s1", TargetSessionID: "s1"}}
	h, _ := replyServer(t, svc)
	req := httptest.NewRequest(http.MethodGet, "/messages/r1/delivery", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var got RoutingReplyDelivery
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.State != "undeliverable" || got.Reason != "session_ended_no_binding" {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/messages/nope/delivery", nil))
	if missing.Code != 404 || errorCode(t, missing) != CodeNotFound {
		t.Fatalf("unknown reply: %d %s", missing.Code, missing.Body)
	}
	wrong := httptest.NewRecorder()
	h.ServeHTTP(wrong, httptest.NewRequest(http.MethodPost, "/messages/r1/delivery", nil))
	if wrong.Code != 405 {
		t.Fatalf("POST delivery: %d", wrong.Code)
	}
}

func TestReplyRoutesAre404WithoutTheService(t *testing.T) {
	h, db := replyServer(t, nil)
	parent := routedMessage(t, db)
	if w := post(h, "/messages/"+parent.ID+"/reply?as=msg://user/local/chris", map[string]any{"body": "x"}, nil); w.Code != 404 {
		t.Fatalf("reply: %d", w.Code)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/messages/x/delivery", nil))
	if w.Code != 404 {
		t.Fatalf("delivery: %d", w.Code)
	}
}

func TestOtherActionsOnAChannelMessageStayGuarded(t *testing.T) {
	svc := &fakeReplies{}
	h, db := replyServer(t, svc)
	parent := routedMessage(t, db)
	// The reply exemption must not widen the mailbox guard to other actions.
	w := post(h, "/messages/"+parent.ID+"/consume?as="+parent.To.URN(), map[string]any{}, nil)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != CodeChannelNotMailbox {
		t.Fatalf("consume on a channel message: %d %s", w.Code, w.Body)
	}
}
