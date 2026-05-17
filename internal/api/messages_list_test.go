package api

// messages_list_test.go — HTTP coverage for the non-destructive inbox
// surface added in CW-20260517-0003: GET /messages/list, POST
// /messages/{id}/read, /archive, /unarchive, and DELETE /messages/{id}.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hollis-labs/go-messaging"
)

func listTestAddr(id string) messaging.Address {
	return messaging.Address{Kind: messaging.KindAgent, Authority: "list-test", ID: id}
}

// listMessages decodes GET /messages/list and returns the message slice.
func listMessages(t *testing.T, base, query string) []map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/messages/list?" + query)
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET list status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
		Count    int              `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if body.Count != len(body.Messages) {
		t.Errorf("count = %d, len(messages) = %d", body.Count, len(body.Messages))
	}
	return body.Messages
}

// postAction fires POST base+path and returns the status code.
func postAction(t *testing.T, base, path string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestMessagesList_HTTPSurface(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ms := db.MessagingStore()
	ctx := context.Background()
	to := listTestAddr("alice")

	var ids []string
	for i := 0; i < 2; i++ {
		sent, err := ms.Send(ctx, messaging.Envelope{
			Kind:    messaging.MsgKindNotice,
			From:    listTestAddr("sender"),
			To:      to,
			Payload: []byte(`{"subject":"hi","body":"there"}`),
		})
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		ids = append(ids, sent.ID)
	}

	// List is non-destructive: returns both, delivered_at null, projection set.
	msgs := listMessages(t, srv.URL, "to="+to.URN())
	if len(msgs) != 2 {
		t.Fatalf("list: got %d, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m["delivered_at"] != nil {
			t.Errorf("list message has delivered_at set — must be non-destructive")
		}
		if m["subject"] != "hi" || m["body"] != "there" {
			t.Errorf("payload projection: subject=%v body=%v, want hi/there", m["subject"], m["body"])
		}
	}

	// Mark one read; unread_only then excludes it.
	if code := postAction(t, srv.URL, "/messages/"+ids[0]+"/read?as="+to.URN()); code != http.StatusNoContent {
		t.Fatalf("POST read = %d, want 204", code)
	}
	if got := listMessages(t, srv.URL, "to="+to.URN()+"&unread_only=true"); len(got) != 1 {
		t.Errorf("unread_only list: got %d, want 1", len(got))
	}

	// Archive via POST; default list drops it, include_archived surfaces it.
	if code := postAction(t, srv.URL, "/messages/"+ids[0]+"/archive?as="+to.URN()); code != http.StatusNoContent {
		t.Fatalf("POST archive = %d, want 204", code)
	}
	if got := listMessages(t, srv.URL, "to="+to.URN()); len(got) != 1 {
		t.Errorf("default list after archive: got %d, want 1", len(got))
	}
	if got := listMessages(t, srv.URL, "to="+to.URN()+"&include_archived=true"); len(got) != 2 {
		t.Errorf("include_archived list: got %d, want 2", len(got))
	}

	// Unarchive restores it.
	if code := postAction(t, srv.URL, "/messages/"+ids[0]+"/unarchive?as="+to.URN()); code != http.StatusNoContent {
		t.Fatalf("POST unarchive = %d, want 204", code)
	}
	if got := listMessages(t, srv.URL, "to="+to.URN()); len(got) != 2 {
		t.Errorf("default list after unarchive: got %d, want 2", len(got))
	}

	// DELETE /messages/{id} is a soft-delete (archive).
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/messages/"+ids[1]+"?as="+to.URN(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", resp.StatusCode)
	}
	if got := listMessages(t, srv.URL, "to="+to.URN()); len(got) != 1 {
		t.Errorf("default list after DELETE: got %d, want 1", len(got))
	}
}

func TestMessagesList_ErrorCases(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ms := db.MessagingStore()
	to := listTestAddr("bob")
	sent, err := ms.Send(context.Background(), messaging.Envelope{
		Kind: messaging.MsgKindNotice, From: listTestAddr("s"), To: to,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// list without ?to → 400.
	resp, _ := http.Get(srv.URL + "/messages/list")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("list without to = %d, want 400", resp.StatusCode)
	}

	// read without ?as → 400.
	if code := postAction(t, srv.URL, "/messages/"+sent.ID+"/read"); code != http.StatusBadRequest {
		t.Errorf("read without as = %d, want 400", code)
	}

	// read with wrong recipient → 409.
	if code := postAction(t, srv.URL, "/messages/"+sent.ID+"/read?as="+listTestAddr("eve").URN()); code != http.StatusConflict {
		t.Errorf("read wrong recipient = %d, want 409", code)
	}

	// read unknown id → 404.
	if code := postAction(t, srv.URL, "/messages/no-such-id/read?as="+to.URN()); code != http.StatusNotFound {
		t.Errorf("read unknown id = %d, want 404", code)
	}
}
