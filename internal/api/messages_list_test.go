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

// listPageBody is the decoded GET /messages/list response.
type listPageBody struct {
	Messages []map[string]any `json:"messages"`
	Count    int              `json:"count"`
	Total    int              `json:"total"`
	Limit    int              `json:"limit"`
	Offset   int              `json:"offset"`
}

// listPage decodes GET /messages/list and returns the full paged response.
func listPage(t *testing.T, base, query string) listPageBody {
	t.Helper()
	resp, err := http.Get(base + "/messages/list?" + query)
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET list status = %d, want 200", resp.StatusCode)
	}
	var body listPageBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if body.Count != len(body.Messages) {
		t.Errorf("count = %d, len(messages) = %d", body.Count, len(body.Messages))
	}
	return body
}

// listMessages decodes GET /messages/list and returns the message slice.
func listMessages(t *testing.T, base, query string) []map[string]any {
	t.Helper()
	return listPage(t, base, query).Messages
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

// TestMessagesList_Pagination covers the limit cap, offset paging, and the
// total count over the HTTP surface.
func TestMessagesList_Pagination(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ms := db.MessagingStore()
	ctx := context.Background()
	to := listTestAddr("paged")

	const n = 150
	for i := 0; i < n; i++ {
		if _, err := ms.Send(ctx, messaging.Envelope{
			Kind: messaging.MsgKindNotice, From: listTestAddr("s"), To: to,
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// Requesting >100 → response is hard-capped at 100, total reports n.
	capped := listPage(t, srv.URL, "to="+to.URN()+"&limit=500")
	if len(capped.Messages) != 100 {
		t.Errorf("limit=500: got %d messages, want 100", len(capped.Messages))
	}
	if capped.Limit != 100 {
		t.Errorf("limit=500: limit field = %d, want 100", capped.Limit)
	}
	if capped.Total != n {
		t.Errorf("limit=500: total = %d, want %d", capped.Total, n)
	}

	// Default (no limit) → 100 returned, limit field 100.
	def := listPage(t, srv.URL, "to="+to.URN())
	if def.Limit != 100 || len(def.Messages) != 100 {
		t.Errorf("default: limit=%d len=%d, want 100/100", def.Limit, len(def.Messages))
	}

	// Offset paging: second page yields the remaining 50.
	page2 := listPage(t, srv.URL, "to="+to.URN()+"&limit=100&offset=100")
	if len(page2.Messages) != 50 {
		t.Errorf("offset=100: got %d messages, want 50", len(page2.Messages))
	}
	if page2.Offset != 100 {
		t.Errorf("offset=100: offset field = %d, want 100", page2.Offset)
	}
	if page2.Total != n {
		t.Errorf("offset=100: total = %d, want %d", page2.Total, n)
	}
}

// TestMessagesList_SurfacesCanceledAt verifies a canceled message is still
// listed and carries a non-null canceled_at over the HTTP surface.
func TestMessagesList_SurfacesCanceledAt(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ms := db.MessagingStore()
	ctx := context.Background()
	to := listTestAddr("cancel-vis")

	live, err := ms.Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice, From: listTestAddr("s"), To: to,
	})
	if err != nil {
		t.Fatalf("send live: %v", err)
	}
	canceled, err := ms.Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice, From: listTestAddr("s"), To: to,
	})
	if err != nil {
		t.Fatalf("send canceled: %v", err)
	}
	if code := postAction(t, srv.URL, "/messages/"+canceled.ID+"/cancel"); code != http.StatusNoContent {
		t.Fatalf("POST cancel = %d, want 204", code)
	}

	msgs := listMessages(t, srv.URL, "to="+to.URN())
	if len(msgs) != 2 {
		t.Fatalf("list after cancel: got %d, want 2 (canceled not filtered)", len(msgs))
	}
	for _, m := range msgs {
		switch m["id"] {
		case canceled.ID:
			if m["canceled_at"] == nil {
				t.Errorf("canceled message: canceled_at = nil, want non-null")
			}
		case live.ID:
			if m["canceled_at"] != nil {
				t.Errorf("live message: canceled_at = %v, want null", m["canceled_at"])
			}
		}
	}
}
