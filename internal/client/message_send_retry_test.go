package client

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// Callers that already set the tether.message_id metadata keep that key: the header
// carries the same value instead of a fresh one that the server would refuse.
func TestMessageSendKeepsLegacyMetadataKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m MessageSendRequest
		_ = json.NewDecoder(r.Body).Decode(&m)
		if r.Header.Get("Idempotency-Key") != m.Metadata["tether.message_id"] {
			http.Error(w, "key mismatch", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{baseURL: srv.URL, http: srv.Client()}
	if _, err := c.MessageSend(context.Background(), MessageSendRequest{Metadata: map[string]string{"tether.message_id": "existing-key"}}); err != nil {
		t.Fatalf("legacy metadata key stopped working: %v", err)
	}
	// An explicit key wins over the metadata; a disagreement reaches the server as is.
	var header string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"m1"}`))
	}))
	t.Cleanup(srv2.Close)
	c = &Client{baseURL: srv2.URL, http: srv2.Client()}
	if _, err := c.MessageSend(context.Background(), MessageSendRequest{IdempotencyKey: "explicit", Metadata: map[string]string{"tether.message_id": "other"}}); err != nil || header != "explicit" {
		t.Fatalf("header = %q err=%v, want explicit", header, err)
	}
}

// A server that committed and then lost or cut off its response is repeated with the
// same key, for both endpoints; a status failure or a malformed body is not repeated.
func TestMessageSendRepeatsAfterALostResponseBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first func(w http.ResponseWriter)
		calls int32
		fails bool
	}{
		{"truncated body", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":`))
		}, 2, false},
		{"body read timeout", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
		}, 2, false},
		{"malformed body is not repeated", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`not json`))
		}, 1, true},
		{"status failure is not repeated", func(w http.ResponseWriter) {
			http.Error(w, "nope", http.StatusConflict)
		}, 1, true},
	} {
		for _, notify := range []bool{false, true} {
			name := tc.name + "/send"
			if notify {
				name = tc.name + "/notify"
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				var mu sync.Mutex
				var keys []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					keys = append(keys, r.Header.Get("Idempotency-Key"))
					mu.Unlock()
					if calls.Add(1) == 1 {
						tc.first(w)
						return
					}
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"m1"}`))
				}))
				t.Cleanup(srv.Close)
				c := &Client{baseURL: srv.URL, http: &http.Client{Timeout: 150 * time.Millisecond}}
				var err error
				if notify {
					_, err = c.MessageNotify(context.Background(), MessageNotifyRequest{})
				} else {
					_, err = c.MessageSend(context.Background(), MessageSendRequest{})
				}
				if (err != nil) != tc.fails || calls.Load() != tc.calls {
					t.Fatalf("calls=%d err=%v, want calls=%d fails=%v", calls.Load(), err, tc.calls, tc.fails)
				}
				mu.Lock()
				defer mu.Unlock()
				if tc.calls == 2 && (keys[0] == "" || keys[0] != keys[1]) {
					t.Fatalf("keys = %q, want the same non-empty key twice", keys)
				}
			})
		}
	}
}
