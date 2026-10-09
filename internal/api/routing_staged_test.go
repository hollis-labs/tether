package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
)

func TestStagedOutputHiddenFromHTTPMailboxReads(t *testing.T) {
	srv, db := newMessageTestServer(t)
	env, err := db.StageTurnOutput(context.Background(), messaging.Envelope{
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, ThreadID: "s1", Payload: []byte(`{"text":"hidden"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/messages/" + env.ID + "?as=" + url.QueryEscape(env.From.URN()),
		"/messages/" + env.ID + "?as=" + url.QueryEscape(env.To.URN())} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("Get %s = %d", path, resp.StatusCode)
		}
	}
	q := "to=" + url.QueryEscape(env.To.URN()) + "&as=" + url.QueryEscape(env.To.URN())
	if page := listPage(t, srv.URL, q); page.Total != 0 || page.Count != 0 {
		t.Fatalf("List exposed stage: %+v", page)
	}
	for _, path := range []string{"/messages/inbox?" + q, "/messages/thread/s1?as=" + url.QueryEscape(env.To.URN())} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Messages []messaging.Envelope `json:"messages"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || len(body.Messages) != 0 {
			t.Fatalf("%s exposed stage: %+v status %d error %v", path, body, resp.StatusCode, err)
		}
	}
}
