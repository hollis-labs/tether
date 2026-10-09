package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func TestChannelPublishPolicyCoversAllCreationPaths(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	address, _ := channels.ChannelAddress("ops")
	from, _ := gomsg.ParseURN("msg://session/local/sender")
	env := gomsg.Envelope{To: address, From: from, Kind: gomsg.MsgKindRequest}
	calls := 0
	svc := channels.New(db, func(ctx context.Context, op, name string, p identity.Principal, claimed gomsg.Address) error {
		if op != "publish" {
			return nil
		}
		calls++
		if claimed != from || name != "ops" || p.ID != "verified" {
			t.Errorf("policy op=%s name=%s caller=%+v from=%+v", op, name, p, claimed)
		}
		return channels.ErrForbidden
	})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "verified"})
	if _, err := svc.Publish(ctx, env); !errors.Is(err, channels.ErrForbidden) {
		t.Fatalf("service: %v", err)
	}
	if _, err := db.MessagingStore().Send(ctx, env); !errors.Is(err, channels.ErrForbidden) {
		t.Fatalf("store: %v", err)
	}
	handler := NewHandler(Deps{Channels: svc, MessageStore: db.MessagingStore()})
	for _, path := range []string{"/messages", "/messages/request?timeout=1ms"} {
		body, _ := json.Marshal(env)
		req := httptest.NewRequest("POST", path, bytes.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("%s status=%d body=%s", path, w.Code, w.Body)
		}
	}
	if calls != 4 {
		t.Fatalf("hook calls=%d", calls)
	}
	var count int
	if err := db.DB().QueryRow(`SELECT count(*) FROM channel_publications`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied publication inserted: %d %v", count, err)
	}
	// Observe-mode permits daemon-internal publishers without HTTP identity.
	db.SetChannelAuthorization(nil)
	if _, err := db.MessagingStore().Send(context.Background(), env); err != nil {
		t.Fatalf("internal publisher: %v", err)
	}
}

func TestChannelMailboxHTTPRejectsWithoutChanges(t *testing.T) {
	srv, db := channelServer(t)
	msg := publishHTTPChannel(t, srv, "ops")
	as := url.QueryEscape(msg.To.URN())
	for _, path := range []string{
		"/messages/inbox?to=" + as + "&as=" + as,
		"/messages/list?to=" + as + "&as=" + as,
		"/messages/" + msg.ID + "/consume?as=" + as,
		"/messages/" + msg.ID + "/cancel?as=" + as,
		"/messages/" + msg.ID + "/archive?as=" + as,
		"/messages/" + msg.ID + "/read?as=" + as,
		"/messages/" + msg.ID + "/redrive?as=" + as,
		"/messages/notify",
	} {
		method := "POST"
		if strings.Contains(path, "/inbox?") || strings.Contains(path, "/list?") {
			method = "GET"
		}
		body, _ := json.Marshal(msg)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var failure ErrorResponse
		err = json.NewDecoder(resp.Body).Decode(&failure)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 400 || failure.Error.Code != "channel_not_mailbox" {
			t.Errorf("%s status=%d error=%+v decode=%v", path, resp.StatusCode, failure, err)
		}
	}
	var count int
	if err := db.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d %v", count, err)
	}
	page := readChannelPage(t, srv, "/channels/ops/messages?as="+url.QueryEscape(channelCaller))
	if len(page.Messages) != 1 || page.Messages[0].DeliveredAt != nil || page.Messages[0].ConsumedAt != nil || string(page.Messages[0].Payload) != string(msg.Payload) {
		t.Fatalf("changed history=%+v", page)
	}
}

func TestChannelLatestHistoryAndLiveCheckpoint(t *testing.T) {
	srv, _ := channelServer(t)
	var messages []gomsg.Envelope
	for range 4 {
		messages = append(messages, publishHTTPChannel(t, srv, "ops"))
	}
	base := "/channels/ops/messages?as=" + url.QueryEscape(channelCaller)
	page := readChannelPage(t, srv, base+"&last=2")
	if len(page.Messages) != 2 || page.Messages[0].ID != messages[2].ID || page.Messages[1].ID != messages[3].ID {
		t.Fatalf("latest=%+v", page)
	}
	for _, suffix := range []string{"&last=0", "&last=1001", "&last=2&since=0", "&last=2&limit=1"} {
		resp, err := http.Get(srv.URL + base + suffix)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s=%d", suffix, resp.StatusCode)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/channels/ops/subscribe?as="+url.QueryEscape(channelCaller), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() || scanner.Text() != "id: "+strconv.FormatInt(page.NextSince, 10) {
		t.Fatalf("live checkpoint=%q err=%v", scanner.Text(), scanner.Err())
	}
	resp.Body.Close()
	// A drop before any publication can resume using the initial checkpoint.
	newMsg := publishHTTPChannel(t, srv, "ops")
	req, _ = http.NewRequestWithContext(ctx, "GET", req.URL.String(), nil)
	req.Header.Set("Last-Event-ID", strconv.FormatInt(page.NextSince, 10))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, msg := sseMessage(t, bufio.NewScanner(resp.Body))
	if msg.ID != newMsg.ID {
		t.Fatalf("checkpoint reconnect=%+v", msg)
	}
}

func TestChannelReconnectSinceOneLastEventThree(t *testing.T) {
	srv, _ := channelServer(t)
	for range 3 {
		publishHTTPChannel(t, srv, "ops")
	}
	last := publishHTTPChannel(t, srv, "ops")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/channels/ops/subscribe?as="+url.QueryEscape(channelCaller)+"&since=1", nil)
	req.Header.Set("Last-Event-ID", "3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, msg := sseMessage(t, bufio.NewScanner(resp.Body))
	if msg.Seq != 4 || msg.ID != last.ID {
		t.Fatalf("reconnect replayed seq<=3: %+v", msg)
	}
}

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
	req, _ = http.NewRequestWithContext(ctx, "GET", srv.URL+"/channels/ops/subscribe?as="+url.QueryEscape(channelCaller)+"&since=1", nil)
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
		{"GET", "/channels/ops/subscribe?as=" + url.QueryEscape(channelCaller) + "&since=9999", 200},
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
	svc := channels.New(db, func(ctx context.Context, operation, name string, p identity.Principal, from gomsg.Address) error {
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
