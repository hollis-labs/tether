package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMailboxWatchIdleStreamEnds(t *testing.T) {
	old := mailboxWatchIdle
	mailboxWatchIdle = 100 * time.Millisecond
	defer func() { mailboxWatchIdle = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	events, errCh, err := c.MailboxWatch(context.Background(), MailboxWatchQuery{To: "msg://agent/a/b"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("silent stream was not abandoned")
	}
	if e := <-errCh; e == nil {
		t.Fatal("want an error so the caller reconnects")
	}
}

func TestMailboxWatchStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	_, _, err := c.MailboxWatch(context.Background(), MailboxWatchQuery{To: "msg://agent/a/b"})
	var se *MailboxWatchStatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway {
		t.Fatalf("err = %v", err)
	}
}
