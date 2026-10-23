package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// CW-20261023-0018: a send whose first attempt times out is repeated with the same
// Idempotency-Key, so a server that had already committed returns the stored message.
func TestMessageSendRepeatsWithTheSameKeyAfterATimeout(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(keys)
		mu.Unlock()
		if n == 1 {
			time.Sleep(300 * time.Millisecond) // longer than the client timeout
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{baseURL: srv.URL, http: &http.Client{Timeout: 100 * time.Millisecond}}

	sent, err := c.MessageSend(context.Background(), MessageSendRequest{Kind: "notice", From: "msg://agent/a/b", To: "msg://agent/a/c"})
	if err != nil || sent.ID != "m1" {
		t.Fatalf("send: %+v %v", sent, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("keys across attempts = %q, want two equal non-empty keys", keys)
	}
}

func TestMessageSendKeepsACallerKeyAndDoesNotRepeatARefusedConnection(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	}))
	c := &Client{baseURL: srv.URL, http: srv.Client()}
	if _, err := c.MessageNotify(context.Background(), MessageNotifyRequest{MessageSendRequest: MessageSendRequest{IdempotencyKey: "mine"}}); err != nil || got != "mine" {
		t.Fatalf("notify key = %q err=%v, want mine", got, err)
	}
	srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c = &Client{baseURL: "http://" + addr, http: &http.Client{Timeout: time.Second}}
	start := time.Now()
	if _, err := c.MessageSend(context.Background(), MessageSendRequest{}); err == nil {
		t.Fatal("send to a closed port succeeded")
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatalf("a refused connection was repeated (took %s)", time.Since(start))
	}
}
