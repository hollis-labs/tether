package workerruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// CW-20261114-0056: the worker hello carries the start-up restore counts.
func TestHelloCarriesRestoreCounts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hello := make(chan workerwire.Frame, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var f workerwire.Frame
		if ws.ReadJSON(&f) == nil {
			hello <- f
		}
		<-ctx.Done()
	}))
	defer func() { cancel(); server.Close() }()
	dir := t.TempDir()
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device.token"), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	counts := &workerwire.RestoreCounts{Live: 3, DeadSkipped: 300, Pruned: 150, Failed: 1}
	w := &Worker{Root: dir, Config: Config{Hub: server.URL, Label: "probe"}, Caps: func(context.Context) json.RawMessage { return json.RawMessage(`{}`) },
		Command: func(context.Context, workerwire.Frame) error { return nil }, Restore: func() *workerwire.RestoreCounts { return counts }}
	go func() { _ = w.Run(ctx) }()
	select {
	case f := <-hello:
		if f.T != "hello" || f.Restore == nil || *f.Restore != *counts {
			t.Fatalf("hello = %+v restore %+v", f.T, f.Restore)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no hello")
	}
}
