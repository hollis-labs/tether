package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func TestRecordLaunchTaskRefs(t *testing.T) {
	var mu sync.Mutex
	var got []api.SessionRefAttachRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions/s1/refs" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req api.SessionRefAttachRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"inserted":true}`))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := client.New("tcp:127.0.0.1:"+port, client.WithToken("t"))
	recordLaunchTaskRefs(context.Background(), c, "s1", []string{"CW-1", "", "CW-2"})
	if len(got) != 2 || got[0].Kind != "torque_task" || got[0].RefID != "CW-1" || got[1].RefID != "CW-2" {
		t.Fatalf("attached = %+v", got)
	}
}
