package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

const channelCaller = "msg://user/local/observer"

func channelServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{Channels: channels.New(db, nil), MessageStore: db.MessagingStore(), Retention: db}))
	t.Cleanup(srv.Close)
	return srv, db
}

func publishHTTPChannel(t *testing.T, srv *httptest.Server, name string) gomsg.Envelope {
	t.Helper()
	address, err := channels.ChannelAddress(name)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"to": address.URN(), "from": "msg://session/local/sender", "kind": "notice", "payload": map[string]string{"text": "hello"}})
	resp, err := http.Post(srv.URL+"/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("send status=%d", resp.StatusCode)
	}
	var env gomsg.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if string(env.Channel) != name || env.To != address {
		t.Fatalf("publication=%+v", env)
	}
	return env
}

func readChannelPage(t *testing.T, srv *httptest.Server, path string) channels.Page {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("history status=%d", resp.StatusCode)
	}
	var page channels.Page
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	return page
}

func sseMessage(t *testing.T, scanner *bufio.Scanner) (string, channels.Message) {
	t.Helper()
	var id string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			id = strings.TrimPrefix(line, "id: ")
		}
		if strings.HasPrefix(line, "data: ") {
			var msg channels.Message
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &msg); err != nil {
				t.Fatal(err)
			}
			return id, msg
		}
	}
	t.Fatalf("SSE ended: %v", scanner.Err())
	return "", channels.Message{}
}

func TestChannelsHTTPHistoryAndSSEReplayThenLive(t *testing.T) {
	srv, _ := channelServer(t)
	first := publishHTTPChannel(t, srv, "ops")
	second := publishHTTPChannel(t, srv, "ops")
	publishHTTPChannel(t, srv, "other")
	historyPath := "/channels/ops/messages?as=" + url.QueryEscape(channelCaller)
	page := readChannelPage(t, srv, historyPath+"&limit=1")
	if page.Name != "ops" || page.Address != first.To.URN() || len(page.Messages) != 1 || page.Messages[0].ID != first.ID {
		t.Fatalf("page=%+v", page)
	}
	next := readChannelPage(t, srv, historyPath+"&since="+strconv.FormatInt(page.NextSince, 10))
	if len(next.Messages) != 1 || next.Messages[0].ID != second.ID {
		t.Fatalf("next=%+v", next)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/channels/ops/subscribe?as="+url.QueryEscape(channelCaller)+"&since=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	scanner := bufio.NewScanner(resp.Body)
	for _, env := range []gomsg.Envelope{first, second} {
		id, msg := sseMessage(t, scanner)
		if msg.ID != env.ID || id != strconv.FormatInt(msg.Seq, 10) {
			t.Fatalf("SSE=%s %+v", id, msg)
		}
	}
	live := publishHTTPChannel(t, srv, "ops")
	id, msg := sseMessage(t, scanner)
	if msg.ID != live.ID {
		t.Fatalf("live=%+v", msg)
	}
	// Browser reconnection resumes after the last received event, across a new connection.
	resp.Body.Close()
	nextLive := publishHTTPChannel(t, srv, "ops")
	req, _ = http.NewRequestWithContext(ctx, "GET", srv.URL+"/channels/ops/subscribe?as="+url.QueryEscape(channelCaller), nil)
	req.Header.Set("Last-Event-ID", id)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	_, msg = sseMessage(t, bufio.NewScanner(resp2.Body))
	if msg.ID != nextLive.ID {
		t.Fatalf("reconnect=%+v", msg)
	}
}

func TestChannelsHTTPDiscoveryAndValidation(t *testing.T) {
	srv, _ := channelServer(t)
	publishHTTPChannel(t, srv, "ops")
	resp, err := http.Get(srv.URL + "/channels?as=" + url.QueryEscape(channelCaller))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Channels []channels.Channel `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(result.Channels) != 1 || result.Channels[0].Name != "ops" {
		t.Fatalf("list=%+v", result)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/channels", 400},
		{"GET", "/channels/ops/messages?as=bad", 400},
		{"GET", "/channels/ops/messages?as=" + url.QueryEscape(channelCaller) + "&since=-1", 400},
		{"GET", "/channels/ops/messages?as=" + url.QueryEscape(channelCaller) + "&limit=1001", 400},
		{"GET", "/channels/bad%20name/messages?as=" + url.QueryEscape(channelCaller), 400},
		{"GET", "/channels/ops/subscribe?as=" + url.QueryEscape(channelCaller) + "&since=9999", 400},
		{"POST", "/channels", 405},
		{"POST", "/channels/ops/messages", 405},
		{"GET", "/channels/ops/unknown", 404},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Errorf("%s %s=%d want %d", tc.method, tc.path, response.StatusCode, tc.status)
		}
	}
	empty := readChannelPage(t, srv, "/channels/new/messages?as="+url.QueryEscape(channelCaller))
	if len(empty.Messages) != 0 || empty.Name != "new" || empty.Address != "msg://service/local/channel/new" {
		t.Fatalf("empty=%+v", empty)
	}
}

func TestChannelsHTTPVerifiedCallerAndPublishPolicy(t *testing.T) {
	_, db := channelServer(t)
	svc := channels.New(db, func(ctx context.Context, operation, name string, p identity.Principal) error {
		if p.ID != "verified" {
			t.Errorf("caller=%+v", p)
		}
		if operation == "publish" {
			return channels.ErrForbidden
		}
		return nil
	})
	h := NewHandler(Deps{Channels: svc, MessageStore: db.MessagingStore()})
	req := httptest.NewRequest("GET", "/channels/ops/messages", nil)
	req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{ID: "verified"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("verified history=%d %s", w.Code, w.Body.String())
	}
	body := `{"kind":"notice","from":"msg://session/local/sender","to":"msg://service/local/channel/ops"}`
	req = httptest.NewRequest("POST", "/messages", strings.NewReader(body))
	req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{ID: "verified"}))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("publish policy=%d %s", w.Code, w.Body.String())
	}
}
