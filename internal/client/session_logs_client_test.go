package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
)

func TestSessionLogClientAuthenticationAndBytes(t *testing.T) {
	data := []byte{0xff, 0, 'x'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer device-test" || r.URL.Path != "/sessions/s/log" || r.URL.Query().Get("offset") != "7" || r.URL.Query().Get("generation") != "generation-test" {
			t.Errorf("wrong authenticated request: %s %s", r.Method, r.URL)
			w.WriteHeader(403)
			return
		}
		_ = json.NewEncoder(w).Encode(api.SessionLogResponse{Data: data, Offset: 7, NextOffset: 10, Size: 10, Generation: "generation-test"})
	}))
	defer server.Close()
	c := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithToken("device-test"))
	offset := int64(7)
	res, err := c.SessionLog(context.Background(), "s", SessionLogOptions{Offset: &offset, Limit: 3, Generation: "generation-test"})
	if err != nil || !bytes.Equal(res.Data, data) || res.NextOffset != 10 {
		t.Fatalf("lost log bytes/range: %+v %v", res, err)
	}
}

func TestSessionLogClientPropagatesFailuresAndCancellation(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"log_changed","message":"unavailable"}}`))
		}))
		c := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithToken("device-test"))
		_, err := c.SessionLog(context.Background(), "s", SessionLogOptions{})
		if err == nil || errors.Is(err, ErrDaemonUnreachable) {
			t.Fatalf("HTTP %d became fallback: %v", status, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = c.SessionLog(ctx, "s", SessionLogOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		server.Close()
	}
	c := New("tcp:127.0.0.1:1", WithToken("device-test"))
	for _, id := range []string{"", "..", "s/../../logs/daemon", "s\\log"} {
		if _, err := c.SessionLog(context.Background(), id, SessionLogOptions{}); err == nil || errors.Is(err, ErrDaemonUnreachable) {
			t.Fatalf("invalid id reached transport: %q %v", id, err)
		}
	}
}
